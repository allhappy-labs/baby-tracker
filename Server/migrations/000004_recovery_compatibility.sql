ALTER TABLE server_state
    ADD CONSTRAINT server_state_recovery_epoch_wire CHECK (recovery_epoch <= 9007199254740991),
    ADD COLUMN generation bigint NOT NULL DEFAULT 1 CHECK (generation = 1),
    ADD COLUMN revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0 AND revision <= 9007199254740991),
    ADD COLUMN event_hash bytea NOT NULL DEFAULT decode(repeat('00', 32), 'hex')
        CHECK (octet_length(event_hash) = 32),
    ADD CONSTRAINT server_state_head_shape CHECK (
        (revision = 0 AND event_hash = decode(repeat('00', 32), 'hex')) OR
        (revision > 0 AND event_hash <> decode(repeat('00', 32), 'hex'))
    );

CREATE TABLE schema_compatibility (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    current_version bigint NOT NULL CHECK (current_version > 0),
    introduced_by text NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_compatibility (singleton, current_version, introduced_by)
VALUES (true, 4, '000004_recovery_compatibility.sql');

CREATE TABLE schema_privilege_manifest (
    schema_version bigint NOT NULL CHECK (schema_version >= 4),
    principal text NOT NULL CHECK (principal IN ('baby_tracker_runtime', 'baby_tracker_projector')),
    object_kind text NOT NULL CHECK (object_kind IN ('table', 'schema', 'sequence', 'column', 'function')),
    object_identity text NOT NULL CHECK (object_identity <> ''),
    privileges text NOT NULL CHECK (privileges <> ''),
    PRIMARY KEY (schema_version, principal, object_kind, object_identity)
);

INSERT INTO schema_privilege_manifest
    (schema_version, principal, object_kind, object_identity, privileges)
VALUES
    (4, 'baby_tracker_runtime', 'schema', 'public', 'USAGE'),
    (4, 'baby_tracker_runtime', 'schema', 'app', 'USAGE'),
    (4, 'baby_tracker_runtime', 'table', 'public.accounts', 'SELECT,INSERT,UPDATE,DELETE'),
    (4, 'baby_tracker_runtime', 'table', 'public.families', 'SELECT,INSERT,UPDATE,DELETE'),
    (4, 'baby_tracker_runtime', 'table', 'public.memberships', 'SELECT,INSERT,UPDATE,DELETE'),
    (4, 'baby_tracker_runtime', 'table', 'public.schema_migrations', 'SELECT'),
    (4, 'baby_tracker_runtime', 'table', 'public.server_state', 'SELECT'),
    (4, 'baby_tracker_runtime', 'table', 'public.schema_compatibility', 'SELECT'),
    (4, 'baby_tracker_runtime', 'table', 'public.schema_privilege_manifest', 'SELECT'),
    (4, 'baby_tracker_runtime', 'function', 'app.current_account_id()', 'EXECUTE'),
    (4, 'baby_tracker_runtime', 'function', 'app.current_family_id()', 'EXECUTE'),
    (4, 'baby_tracker_projector', 'schema', 'public', 'USAGE'),
    (4, 'baby_tracker_projector', 'schema', 'app', 'USAGE'),
    (4, 'baby_tracker_projector', 'table', 'public.security_projection_head', 'SELECT'),
    (4, 'baby_tracker_projector', 'table', 'public.server_state', 'SELECT'),
    (4, 'baby_tracker_projector', 'table', 'public.schema_compatibility', 'SELECT'),
    (4, 'baby_tracker_projector', 'table', 'public.schema_privilege_manifest', 'SELECT'),
    (4, 'baby_tracker_projector', 'function', 'app.project_security_event(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)', 'EXECUTE'),
    (4, 'baby_tracker_projector', 'function', 'app.update_server_state(uuid,bigint,bigint,bigint,bytea)', 'EXECUTE');

CREATE OR REPLACE FUNCTION app.update_server_state(
    p_instance_id uuid,
    p_recovery_epoch bigint,
    p_generation bigint,
    p_revision bigint,
    p_event_hash bytea
) RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
DECLARE
    affected_rows bigint;
BEGIN
    IF p_instance_id IS NULL OR p_instance_id = '00000000-0000-0000-0000-000000000000'
       OR p_recovery_epoch IS NULL OR p_recovery_epoch <= 0 OR p_recovery_epoch > 9007199254740991
       OR p_generation IS DISTINCT FROM 1
       OR p_revision IS NULL OR p_revision < 0 OR p_revision > 9007199254740991
       OR octet_length(p_event_hash) IS DISTINCT FROM 32
       OR ((p_revision = 0) IS DISTINCT FROM (p_event_hash = decode(repeat('00', 32), 'hex'))) THEN
        RAISE EXCEPTION 'invalid server state mirror' USING ERRCODE = '22023';
    END IF;

    INSERT INTO public.server_state
        (singleton, instance_id, recovery_epoch, generation, revision, event_hash, updated_at)
    VALUES
        (true, p_instance_id, p_recovery_epoch, p_generation, p_revision, p_event_hash, now())
    ON CONFLICT (singleton) DO UPDATE SET
        recovery_epoch = EXCLUDED.recovery_epoch,
        generation = EXCLUDED.generation,
        revision = EXCLUDED.revision,
        event_hash = EXCLUDED.event_hash,
        updated_at = now()
    WHERE public.server_state.instance_id = EXCLUDED.instance_id
      AND (
          public.server_state.recovery_epoch < EXCLUDED.recovery_epoch OR
          (public.server_state.recovery_epoch = EXCLUDED.recovery_epoch
           AND public.server_state.generation = EXCLUDED.generation
           AND (
               public.server_state.revision < EXCLUDED.revision OR
               (public.server_state.revision = EXCLUDED.revision
                AND public.server_state.event_hash = EXCLUDED.event_hash)
           ))
      );
    GET DIAGNOSTICS affected_rows = ROW_COUNT;
    IF affected_rows <> 1 THEN
        RAISE EXCEPTION 'server identity or epoch conflict' USING ERRCODE = '40001';
    END IF;
END;
$$;

REVOKE ALL ON schema_compatibility FROM PUBLIC;
REVOKE ALL ON schema_privilege_manifest FROM PUBLIC;
REVOKE ALL ON FUNCTION app.update_server_state(uuid, bigint, bigint, bigint, bytea) FROM PUBLIC;
GRANT SELECT ON schema_compatibility, schema_privilege_manifest, server_state TO baby_tracker_runtime, baby_tracker_projector;
GRANT EXECUTE ON FUNCTION app.update_server_state(uuid, bigint, bigint, bigint, bytea) TO baby_tracker_projector;
