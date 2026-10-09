-- Expose only the initial sign-in time for a session lineage. The runtime
-- uses it to enforce the current self-hosted age on sessions issued by an
-- older server without rewriting the immutable security ledger.
CREATE FUNCTION app.session_lineage_started_at(p_account_id uuid, p_root_session_id uuid)
RETURNS timestamptz
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT s.created_at FROM public.sessions s
 WHERE s.session_id=p_root_session_id
   AND s.lineage_root_session_id=p_root_session_id
   AND s.account_id=p_account_id
$$;
REVOKE ALL ON FUNCTION app.session_lineage_started_at(uuid,uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.session_lineage_started_at(uuid,uuid) TO baby_tracker_runtime;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 20,principal,object_kind,object_identity,privileges
 FROM public.schema_privilege_manifest WHERE schema_version=19;
INSERT INTO public.schema_privilege_manifest VALUES
 (20,'baby_tracker_runtime','function','app.session_lineage_started_at(uuid,uuid)','EXECUTE');
UPDATE public.schema_compatibility SET current_version=20,
 introduced_by='000020_session_lineage_start.sql',recorded_at=now() WHERE singleton;
