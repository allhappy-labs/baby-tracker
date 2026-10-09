-- Owner family exports. Ciphertext and wrapped keys live only in these tables.
ALTER TABLE public.schema_privilege_manifest DROP CONSTRAINT schema_privilege_manifest_principal_check;
ALTER TABLE public.schema_privilege_manifest ADD CONSTRAINT schema_privilege_manifest_principal_check
    CHECK(principal IN ('baby_tracker_runtime','baby_tracker_projector','baby_tracker_apple_revoker','baby_tracker_maintenance'));
GRANT USAGE ON SCHEMA public TO baby_tracker_maintenance;
CREATE TABLE public.export_jobs (
    id uuid PRIMARY KEY,
    family_id uuid NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    requester_account_id uuid NOT NULL REFERENCES public.accounts(id),
    request_id uuid NOT NULL,
    server_instance_id uuid NOT NULL,
    recovery_epoch bigint NOT NULL CHECK (recovery_epoch BETWEEN 1 AND 9007199254740991),
    security_revision bigint NOT NULL CHECK (security_revision BETWEEN 1 AND 9007199254740991),
    state text NOT NULL CHECK (state IN ('queued','running','ready','failed','expired')),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt BETWEEN 0 AND 5),
    lease_id uuid,
    lease_until timestamptz,
    last_code text CHECK (last_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    header jsonb,
    plaintext_bytes bigint CHECK (plaintext_bytes BETWEEN 0 AND 52428800),
    plaintext_sha256 text CHECK (plaintext_sha256 ~ '^[0-9a-f]{64}$'),
    CHECK (expires_at = created_at + interval '24 hours'),
    CHECK ((lease_id IS NULL) = (lease_until IS NULL)),
    CHECK ((state = 'ready') = (header IS NOT NULL)),
    CHECK (state <> 'ready' OR (plaintext_bytes IS NOT NULL AND plaintext_sha256 IS NOT NULL)),
    UNIQUE (requester_account_id, request_id),
    UNIQUE (id, attempt),
    UNIQUE (id, family_id)
);
CREATE UNIQUE INDEX export_jobs_one_active_per_family
    ON public.export_jobs(family_id) WHERE state IN ('queued','running','ready');
CREATE INDEX export_jobs_claim_idx ON public.export_jobs(next_attempt_at,lease_until,id)
    WHERE state IN ('queued','running');
CREATE INDEX export_jobs_expiry_idx ON public.export_jobs(expires_at,id);

CREATE TABLE public.export_chunks (
    family_id uuid NOT NULL,
    export_id uuid NOT NULL,
    attempt integer NOT NULL,
    chunk_index bigint NOT NULL CHECK (chunk_index >= 0),
    plaintext_length integer NOT NULL CHECK (plaintext_length BETWEEN 0 AND 65536),
    is_final boolean NOT NULL,
    ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) = plaintext_length + 16),
    PRIMARY KEY (export_id,attempt,chunk_index),
    FOREIGN KEY (export_id,attempt) REFERENCES public.export_jobs(id,attempt) ON DELETE CASCADE,
    FOREIGN KEY (export_id,family_id) REFERENCES public.export_jobs(id,family_id) ON DELETE CASCADE
);

-- A reservation outlives failed jobs and removed ciphertext. The account and
-- family rows are locked before checking these scopes in the admission path.
CREATE TABLE public.export_rate_reservations (
    scope text NOT NULL CHECK (scope IN ('account','family')),
    key_id uuid NOT NULL,
    request_id uuid NOT NULL,
    accepted_at timestamptz NOT NULL,
    PRIMARY KEY (scope,key_id,request_id)
);
CREATE INDEX export_rate_recent_idx ON public.export_rate_reservations(scope,key_id,accepted_at DESC);

ALTER TABLE public.export_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.export_jobs FORCE ROW LEVEL SECURITY;
CREATE POLICY export_requester ON public.export_jobs TO baby_tracker_runtime
    USING (family_id=app.current_family_id() AND requester_account_id=app.current_account_id())
    WITH CHECK (family_id=app.current_family_id() AND requester_account_id=app.current_account_id());
CREATE POLICY export_worker ON public.export_jobs TO baby_tracker_maintenance USING (true) WITH CHECK (true);
ALTER TABLE public.export_chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.export_chunks FORCE ROW LEVEL SECURITY;
CREATE POLICY export_requester ON public.export_chunks TO baby_tracker_runtime
    USING (family_id=app.current_family_id() AND EXISTS
        (SELECT 1 FROM public.export_jobs j WHERE j.id=export_id AND j.requester_account_id=app.current_account_id()))
    WITH CHECK (false);
CREATE POLICY export_worker ON public.export_chunks TO baby_tracker_maintenance USING (true) WITH CHECK (true);
ALTER TABLE public.export_rate_reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.export_rate_reservations FORCE ROW LEVEL SECURITY;
CREATE POLICY export_requester ON public.export_rate_reservations TO baby_tracker_runtime
    USING ((scope='account' AND key_id=app.current_account_id()) OR (scope='family' AND key_id=app.current_family_id()))
    WITH CHECK ((scope='account' AND key_id=app.current_account_id()) OR (scope='family' AND key_id=app.current_family_id()));
CREATE POLICY export_worker ON public.export_rate_reservations TO baby_tracker_maintenance USING (true) WITH CHECK (true);

REVOKE ALL ON public.export_jobs,public.export_chunks,public.export_rate_reservations FROM PUBLIC;
GRANT SELECT,INSERT ON public.export_jobs TO baby_tracker_runtime;
GRANT SELECT ON public.export_chunks TO baby_tracker_runtime;
GRANT SELECT,INSERT ON public.export_rate_reservations TO baby_tracker_runtime;
GRANT SELECT,UPDATE,DELETE ON public.export_jobs TO baby_tracker_maintenance;
GRANT SELECT,INSERT,DELETE ON public.export_chunks TO baby_tracker_maintenance;
GRANT SELECT,DELETE ON public.export_rate_reservations TO baby_tracker_maintenance;

-- The runtime cannot read raw security audit payloads. Expose only the four
-- archive fields for events whose effects belong to its current family.
CREATE FUNCTION app.export_family_audit(p_family_id uuid)
RETURNS TABLE(revision bigint,kind text,actor_account_id uuid,at text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT a.revision,a.kind,a.actor_account_id,a.event_data->>'at'
 FROM public.security_audit_events a
 WHERE p_family_id=app.current_family_id()
   AND EXISTS (SELECT 1 FROM public.memberships m WHERE m.family_id=p_family_id
       AND m.account_id=app.current_account_id() AND m.role='owner' AND m.status='active')
   AND EXISTS (SELECT 1 FROM jsonb_array_elements(a.event_data->'effects') effect
       WHERE effect->>'family_id'=p_family_id::text)
 ORDER BY a.revision
$$;
REVOKE ALL ON FUNCTION app.export_family_audit(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.export_family_audit(uuid) TO baby_tracker_runtime;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 19,principal,object_kind,object_identity,privileges FROM public.schema_privilege_manifest WHERE schema_version=18;
INSERT INTO public.schema_privilege_manifest VALUES
 (19,'baby_tracker_runtime','table','public.export_jobs','SELECT,INSERT'),
 (19,'baby_tracker_runtime','table','public.export_chunks','SELECT'),
 (19,'baby_tracker_runtime','table','public.export_rate_reservations','SELECT,INSERT'),
 (19,'baby_tracker_runtime','function','app.export_family_audit(uuid)','EXECUTE'),
 (19,'baby_tracker_maintenance','schema','public','USAGE'),
 (19,'baby_tracker_maintenance','table','public.export_jobs','SELECT,UPDATE,DELETE'),
 (19,'baby_tracker_maintenance','table','public.export_chunks','SELECT,INSERT,DELETE'),
 (19,'baby_tracker_maintenance','table','public.export_rate_reservations','SELECT,DELETE');
UPDATE public.schema_compatibility SET current_version=19,introduced_by='000019_exports.sql',recorded_at=now() WHERE singleton;
