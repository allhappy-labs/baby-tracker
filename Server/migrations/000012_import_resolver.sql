-- Durable import staging and final mapping storage. Application code using
-- these tables lands in the following feature commit; this migration is the
-- deployable overlap predecessor for schema 11 -> 12.

CREATE TABLE public.import_sessions (
    family_id uuid NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    import_id uuid NOT NULL CHECK (import_id <> '00000000-0000-0000-0000-000000000000'),
    initiating_account_id uuid NOT NULL CHECK (initiating_account_id <> '00000000-0000-0000-0000-000000000000'),
    initiating_device_id uuid NOT NULL CHECK (initiating_device_id <> '00000000-0000-0000-0000-000000000000'),
    import_mode text NOT NULL CHECK (import_mode IN ('preserve_local_ids', 'match_shared')),
    normalization_version text NOT NULL CHECK (normalization_version = 'nfc-trim-fold-nfc-v1'),
    total_record_count bigint NOT NULL CHECK (total_record_count BETWEEN 0 AND 1000000),
    stored_record_count bigint NOT NULL DEFAULT 0 CHECK (stored_record_count BETWEEN 0 AND 1000000),
    next_chunk_index bigint NOT NULL DEFAULT 0 CHECK (next_chunk_index BETWEEN 0 AND 9007199254740991),
    manifest_complete boolean NOT NULL DEFAULT false,
    finalized_at timestamptz CHECK (finalized_at IS NULL OR isfinite(finalized_at)),
    mapping_complete boolean NOT NULL DEFAULT false,
    mapping_completed_at timestamptz CHECK (mapping_completed_at IS NULL OR isfinite(mapping_completed_at)),
    resolver_revision bigint NOT NULL DEFAULT 1 CHECK (resolver_revision BETWEEN 1 AND 9007199254740991),
    created_at timestamptz NOT NULL DEFAULT now() CHECK (isfinite(created_at)),
    expires_at timestamptz NOT NULL CHECK (isfinite(expires_at) AND expires_at > created_at),
    expired_at timestamptz CHECK (expired_at IS NULL OR isfinite(expired_at)),
    storage_bytes bigint NOT NULL CHECK (storage_bytes BETWEEN 1 AND 9007199254740991),
    PRIMARY KEY (family_id, import_id),
    CONSTRAINT import_sessions_state_shape CHECK (
        stored_record_count <= total_record_count
        AND manifest_complete = (finalized_at IS NOT NULL)
        AND (NOT manifest_complete OR stored_record_count = total_record_count)
        AND mapping_complete = (mapping_completed_at IS NOT NULL)
        AND (NOT mapping_complete OR manifest_complete)
        AND (finalized_at IS NULL OR (finalized_at >= created_at AND finalized_at <= expires_at))
        AND (mapping_completed_at IS NULL OR
            (mapping_completed_at >= finalized_at AND mapping_completed_at <= expires_at))
        AND (expired_at IS NULL OR (NOT mapping_complete AND expired_at >= created_at))
        AND NOT (mapping_complete AND expired_at IS NOT NULL))
);
CREATE INDEX import_sessions_expiry_idx
    ON public.import_sessions(expires_at, family_id, import_id)
    WHERE NOT mapping_complete AND expired_at IS NULL;
CREATE INDEX import_sessions_account_unfinished_idx
    ON public.import_sessions(initiating_account_id, expires_at, family_id, import_id)
    WHERE NOT mapping_complete AND expired_at IS NULL;
CREATE INDEX import_sessions_retained_account_idx
    ON public.import_sessions(family_id, initiating_account_id, import_id);

CREATE TABLE public.import_chunks (
    family_id uuid NOT NULL,
    import_id uuid NOT NULL,
    chunk_index bigint NOT NULL CHECK (chunk_index BETWEEN 0 AND 9007199254740991),
    canonical_sha256 bytea NOT NULL CHECK (octet_length(canonical_sha256) = 32),
    record_count integer NOT NULL CHECK (record_count BETWEEN 0 AND 1000),
    finalize boolean NOT NULL,
    response_status integer NOT NULL CHECK (response_status BETWEEN 200 AND 599),
    response_body bytea NOT NULL CHECK (octet_length(response_body) BETWEEN 1 AND 4194304),
    storage_bytes bigint NOT NULL CHECK (storage_bytes BETWEEN 1 AND 9007199254740991),
    created_at timestamptz NOT NULL DEFAULT now() CHECK (isfinite(created_at)),
    PRIMARY KEY (family_id, import_id, chunk_index),
    CONSTRAINT import_chunks_nonempty_or_final CHECK (record_count > 0 OR finalize),
    CONSTRAINT import_chunks_session_fk FOREIGN KEY (family_id, import_id)
        REFERENCES public.import_sessions(family_id, import_id) ON DELETE CASCADE
);

CREATE TABLE public.import_baby_candidates (
    family_id uuid NOT NULL,
    import_id uuid NOT NULL,
    local_baby_id uuid NOT NULL CHECK (local_baby_id <> '00000000-0000-0000-0000-000000000000'),
    name text NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 400),
    normalized_name text NOT NULL CHECK (octet_length(normalized_name) BETWEEN 1 AND 1600),
    birth_date date NOT NULL CHECK (isfinite(birth_date)
        AND birth_date BETWEEN DATE '1900-01-01' AND DATE '2100-12-31'),
    sort_order integer NOT NULL CHECK (sort_order BETWEEN 0 AND 1000000),
    storage_bytes bigint NOT NULL CHECK (storage_bytes BETWEEN 1 AND 9007199254740991),
    PRIMARY KEY (family_id, import_id, local_baby_id),
    CONSTRAINT import_candidates_session_fk FOREIGN KEY (family_id, import_id)
        REFERENCES public.import_sessions(family_id, import_id) ON DELETE CASCADE
);

CREATE TABLE public.import_record_manifests (
    family_id uuid NOT NULL,
    import_id uuid NOT NULL,
    record_id uuid NOT NULL CHECK (record_id <> '00000000-0000-0000-0000-000000000000'),
    entity_kind text NOT NULL CHECK (entity_kind IN ('feed', 'sleep', 'diaper', 'timer')),
    local_baby_id uuid NOT NULL CHECK (local_baby_id <> '00000000-0000-0000-0000-000000000000'),
    chunk_index bigint NOT NULL CHECK (chunk_index BETWEEN 0 AND 9007199254740991),
    content_sha256 bytea NOT NULL CHECK (octet_length(content_sha256) = 32),
    storage_bytes bigint NOT NULL CHECK (storage_bytes BETWEEN 1 AND 9007199254740991),
    PRIMARY KEY (family_id, import_id, record_id),
    CONSTRAINT import_manifests_session_fk FOREIGN KEY (family_id, import_id)
        REFERENCES public.import_sessions(family_id, import_id) ON DELETE CASCADE,
    CONSTRAINT import_manifests_candidate_fk FOREIGN KEY (family_id, import_id, local_baby_id)
        REFERENCES public.import_baby_candidates(family_id, import_id, local_baby_id),
    CONSTRAINT import_manifests_chunk_fk FOREIGN KEY (family_id, import_id, chunk_index)
        REFERENCES public.import_chunks(family_id, import_id, chunk_index)
);
CREATE INDEX import_manifests_local_baby_idx
    ON public.import_record_manifests(family_id, import_id, local_baby_id, record_id);

CREATE TABLE public.import_conflicts (
    family_id uuid NOT NULL,
    import_id uuid NOT NULL,
    conflict_id uuid NOT NULL CHECK (conflict_id <> '00000000-0000-0000-0000-000000000000'),
    kind text NOT NULL CHECK (kind IN (
        'ambiguous_baby', 'ambiguous_local_babies', 'baby_uuid_collision',
        'record_content_conflict', 'record_deleted_conflict')),
    local_baby_id uuid NOT NULL CHECK (local_baby_id <> '00000000-0000-0000-0000-000000000000'),
    record_id uuid,
    candidate_baby_ids uuid[] NOT NULL DEFAULT '{}'::uuid[],
    candidate_versions bigint[] NOT NULL DEFAULT '{}'::bigint[],
    candidate_fingerprint bytea NOT NULL CHECK (octet_length(candidate_fingerprint) = 32),
    decision_choice text CHECK (decision_choice IN ('use_existing', 'keep_local')),
    decision_canonical_baby_id uuid,
    decision_account_id uuid,
    decision_revision bigint CHECK (decision_revision BETWEEN 1 AND 9007199254740991),
    decided_at timestamptz CHECK (decided_at IS NULL OR isfinite(decided_at)),
    created_revision bigint NOT NULL CHECK (created_revision BETWEEN 1 AND 9007199254740991),
    updated_revision bigint NOT NULL CHECK (updated_revision BETWEEN 1 AND 9007199254740991),
    created_at timestamptz NOT NULL DEFAULT now() CHECK (isfinite(created_at)),
    updated_at timestamptz NOT NULL DEFAULT now() CHECK (isfinite(updated_at)),
    storage_bytes bigint NOT NULL CHECK (storage_bytes BETWEEN 1 AND 9007199254740991),
    PRIMARY KEY (family_id, import_id, conflict_id),
    CONSTRAINT import_conflicts_candidate_shape CHECK (
        cardinality(candidate_baby_ids) = cardinality(candidate_versions)
        AND cardinality(candidate_baby_ids) <= 500
        AND array_position(candidate_baby_ids, NULL) IS NULL
        AND array_position(candidate_versions, NULL) IS NULL
        AND '00000000-0000-0000-0000-000000000000'::uuid <> ALL(candidate_baby_ids)
        AND 1 <= ALL(candidate_versions)
        AND 9007199254740991 >= ALL(candidate_versions)),
    CONSTRAINT import_conflicts_kind_shape CHECK (
        (kind IN ('record_content_conflict', 'record_deleted_conflict')) = (record_id IS NOT NULL)),
    CONSTRAINT import_conflicts_decision_state_shape CHECK (
        (decision_choice IS NULL
            AND decision_canonical_baby_id IS NULL
            AND decision_account_id IS NULL
            AND decision_revision IS NULL
            AND decided_at IS NULL)
        OR ((decision_choice = 'use_existing') IS TRUE
            AND decision_canonical_baby_id IS NOT NULL
            AND decision_account_id IS NOT NULL
            AND decision_revision IS NOT NULL
            AND decided_at IS NOT NULL)
        OR ((decision_choice = 'keep_local') IS TRUE
            AND decision_canonical_baby_id IS NULL
            AND decision_account_id IS NOT NULL
            AND decision_revision IS NOT NULL
            AND decided_at IS NOT NULL)),
    CONSTRAINT import_conflicts_revision_order CHECK (updated_revision >= created_revision),
    CONSTRAINT import_conflicts_session_fk FOREIGN KEY (family_id, import_id)
        REFERENCES public.import_sessions(family_id, import_id) ON DELETE CASCADE,
    CONSTRAINT import_conflicts_candidate_fk FOREIGN KEY (family_id, import_id, local_baby_id)
        REFERENCES public.import_baby_candidates(family_id, import_id, local_baby_id),
    CONSTRAINT import_conflicts_manifest_fk FOREIGN KEY (family_id, import_id, record_id)
        REFERENCES public.import_record_manifests(family_id, import_id, record_id)
);
CREATE INDEX import_conflicts_unresolved_idx
    ON public.import_conflicts(family_id, import_id, conflict_id)
    WHERE decision_choice IS NULL;

CREATE TABLE public.import_conflict_receipts (
    family_id uuid NOT NULL,
    import_id uuid NOT NULL,
    command_id uuid NOT NULL CHECK (command_id <> '00000000-0000-0000-0000-000000000000'),
    account_id uuid NOT NULL CHECK (account_id <> '00000000-0000-0000-0000-000000000000'),
    device_id uuid NOT NULL CHECK (device_id <> '00000000-0000-0000-0000-000000000000'),
    command_sha256 bytea NOT NULL CHECK (octet_length(command_sha256) = 32),
    resolver_revision bigint NOT NULL CHECK (resolver_revision BETWEEN 1 AND 9007199254740991),
    response_status integer NOT NULL CHECK (response_status BETWEEN 200 AND 599),
    response_body bytea NOT NULL CHECK (octet_length(response_body) BETWEEN 1 AND 4194304),
    storage_bytes bigint NOT NULL CHECK (storage_bytes BETWEEN 1 AND 9007199254740991),
    created_at timestamptz NOT NULL DEFAULT now() CHECK (isfinite(created_at)),
    PRIMARY KEY (family_id, import_id, command_id),
    CONSTRAINT import_conflict_receipts_session_fk FOREIGN KEY (family_id, import_id)
        REFERENCES public.import_sessions(family_id, import_id) ON DELETE CASCADE
);

CREATE TABLE public.import_baby_mappings (
    family_id uuid NOT NULL,
    import_id uuid NOT NULL,
    local_baby_id uuid NOT NULL CHECK (local_baby_id <> '00000000-0000-0000-0000-000000000000'),
    shared_baby_id uuid NOT NULL CHECK (shared_baby_id <> '00000000-0000-0000-0000-000000000000'),
    published_revision bigint NOT NULL CHECK (published_revision BETWEEN 1 AND 9007199254740991),
    shared_baby_entity_version bigint NOT NULL CHECK (shared_baby_entity_version BETWEEN 1 AND 9007199254740991),
    shared_baby_cursor bigint NOT NULL CHECK (shared_baby_cursor BETWEEN 1 AND 9007199254740991),
    storage_bytes bigint NOT NULL CHECK (storage_bytes BETWEEN 1 AND 9007199254740991),
    created_at timestamptz NOT NULL DEFAULT now() CHECK (isfinite(created_at)),
    PRIMARY KEY (family_id, import_id, local_baby_id),
    CONSTRAINT import_mappings_candidate_fk FOREIGN KEY (family_id, import_id, local_baby_id)
        REFERENCES public.import_baby_candidates(family_id, import_id, local_baby_id),
    CONSTRAINT import_mappings_session_fk FOREIGN KEY (family_id, import_id)
        REFERENCES public.import_sessions(family_id, import_id) ON DELETE CASCADE,
    CONSTRAINT import_mappings_shared_baby_fk FOREIGN KEY (family_id, shared_baby_id)
        REFERENCES public.babies(family_id, id)
);
CREATE UNIQUE INDEX import_baby_mappings_shared_key
    ON public.import_baby_mappings(family_id, import_id, shared_baby_id);

ALTER TABLE public.import_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.import_sessions FORCE ROW LEVEL SECURITY;
ALTER TABLE public.import_chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.import_chunks FORCE ROW LEVEL SECURITY;
ALTER TABLE public.import_baby_candidates ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.import_baby_candidates FORCE ROW LEVEL SECURITY;
ALTER TABLE public.import_record_manifests ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.import_record_manifests FORCE ROW LEVEL SECURITY;
ALTER TABLE public.import_conflicts ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.import_conflicts FORCE ROW LEVEL SECURITY;
ALTER TABLE public.import_conflict_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.import_conflict_receipts FORCE ROW LEVEL SECURITY;
ALTER TABLE public.import_baby_mappings ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.import_baby_mappings FORCE ROW LEVEL SECURITY;

CREATE POLICY family_context ON public.import_sessions
    USING (family_id = app.current_family_id())
    WITH CHECK (family_id = app.current_family_id());
CREATE POLICY family_context ON public.import_chunks
    USING (family_id = app.current_family_id())
    WITH CHECK (family_id = app.current_family_id());
CREATE POLICY family_context ON public.import_baby_candidates
    USING (family_id = app.current_family_id())
    WITH CHECK (family_id = app.current_family_id());
CREATE POLICY family_context ON public.import_record_manifests
    USING (family_id = app.current_family_id())
    WITH CHECK (family_id = app.current_family_id());
CREATE POLICY family_context ON public.import_conflicts
    USING (family_id = app.current_family_id())
    WITH CHECK (family_id = app.current_family_id());
CREATE POLICY family_context ON public.import_conflict_receipts
    USING (family_id = app.current_family_id())
    WITH CHECK (family_id = app.current_family_id());
CREATE POLICY family_context ON public.import_baby_mappings
    USING (family_id = app.current_family_id())
    WITH CHECK (family_id = app.current_family_id());

CREATE FUNCTION app.import_unfinished_session_count(
    p_account_id uuid, p_now timestamptz
) RETURNS bigint
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = pg_catalog
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

-- Only execution as this exact SECURITY DEFINER function's owner may use the
-- cross-family account policy. The runtime login can SET ROLE to its inherited
-- baby_tracker_runtime group, but it cannot assume the migration/function owner.
CREATE POLICY account_count_context ON public.import_sessions FOR SELECT
    USING (
        current_user = pg_catalog.pg_get_userbyid((
            SELECT p.proowner
              FROM pg_catalog.pg_proc AS p
             WHERE p.oid = 'app.import_unfinished_session_count(uuid,timestamptz)'::pg_catalog.regprocedure
        ))
        AND initiating_account_id = app.current_account_id()
    );

REVOKE ALL ON FUNCTION app.import_unfinished_session_count(uuid,timestamptz)
    FROM PUBLIC, baby_tracker_projector, baby_tracker_apple_revoker;
REVOKE ALL ON public.import_sessions, public.import_chunks,
    public.import_baby_candidates, public.import_record_manifests,
    public.import_conflicts, public.import_conflict_receipts,
    public.import_baby_mappings
    FROM PUBLIC, baby_tracker_runtime, baby_tracker_projector,
    baby_tracker_apple_revoker;
GRANT SELECT, INSERT, UPDATE ON public.import_sessions, public.import_conflicts
    TO baby_tracker_runtime;
GRANT SELECT, INSERT ON public.import_chunks, public.import_baby_candidates,
    public.import_record_manifests, public.import_conflict_receipts,
    public.import_baby_mappings TO baby_tracker_runtime;
GRANT EXECUTE ON FUNCTION app.import_unfinished_session_count(uuid,timestamptz)
    TO baby_tracker_runtime;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
SELECT 12,principal,object_kind,object_identity,privileges
  FROM public.schema_privilege_manifest WHERE schema_version=11;
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges) VALUES
    (12,'baby_tracker_runtime','table','public.import_sessions','SELECT,INSERT,UPDATE'),
    (12,'baby_tracker_runtime','table','public.import_chunks','SELECT,INSERT'),
    (12,'baby_tracker_runtime','table','public.import_baby_candidates','SELECT,INSERT'),
    (12,'baby_tracker_runtime','table','public.import_record_manifests','SELECT,INSERT'),
    (12,'baby_tracker_runtime','table','public.import_conflicts','SELECT,INSERT,UPDATE'),
    (12,'baby_tracker_runtime','table','public.import_conflict_receipts','SELECT,INSERT'),
    (12,'baby_tracker_runtime','table','public.import_baby_mappings','SELECT,INSERT'),
    (12,'baby_tracker_runtime','function','app.import_unfinished_session_count(uuid,timestamp with time zone)','EXECUTE');

UPDATE public.schema_compatibility
   SET current_version=12,introduced_by='000012_import_resolver.sql',recorded_at=now()
 WHERE singleton;
