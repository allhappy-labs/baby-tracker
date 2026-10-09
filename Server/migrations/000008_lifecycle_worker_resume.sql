-- Additive worker-private discovery for durable orphan-cleanup resumption.
-- Migration 000007 is an accepted compatibility predecessor and remains immutable.

CREATE FUNCTION app.claim_unfinished_credential_cleanup(
    p_worker_id text,p_now timestamptz,p_lease_expires_at timestamptz)
RETURNS TABLE(lease_token text,operation_version text,operation_id uuid,command_id uuid,
    account_id uuid,server_instance_id uuid,purpose text,object_id uuid,reference_id text,reference_key_id text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE token text;
BEGIN
    IF p_worker_id !~ '^[A-Za-z0-9._:-]{1,128}$' OR p_now IS NULL OR p_lease_expires_at IS NULL
       OR NOT isfinite(p_now) OR NOT isfinite(p_lease_expires_at) OR p_lease_expires_at<=p_now
       OR p_lease_expires_at>p_now+interval '5 minutes' THEN
        RAISE EXCEPTION 'invalid cleanup claim' USING ERRCODE='22023';
    END IF;
    token:=encode(sha256(convert_to(gen_random_uuid()::text||clock_timestamp()::text||p_worker_id,'UTF8')),'hex');
    RETURN QUERY
    WITH candidate AS (
        SELECT j.account_id,j.credential_object_id,j.credential_reference_id,j.credential_reference_key_id
          FROM public.credential_cleanup_jobs j
         WHERE j.status<>'complete' AND j.next_attempt_at<=p_now
           AND (j.lease_expires_at IS NULL OR j.lease_expires_at<=p_now)
           AND j.operation_version<9007199254740991 AND j.attempts<1000000
         ORDER BY j.next_attempt_at,j.updated_at,j.account_id,j.credential_object_id,
                  j.credential_reference_id,j.credential_reference_key_id
         FOR UPDATE SKIP LOCKED LIMIT 1
    )
    UPDATE public.credential_cleanup_jobs j
       SET lease_owner=p_worker_id,lease_token=token,lease_expires_at=p_lease_expires_at,
           attempts=j.attempts+1,operation_version=j.operation_version+1,updated_at=p_now
      FROM candidate c
     WHERE j.account_id=c.account_id AND j.credential_object_id=c.credential_object_id
       AND j.credential_reference_id=c.credential_reference_id
       AND j.credential_reference_key_id=c.credential_reference_key_id
    RETURNING j.lease_token,j.operation_version::text,j.operation_id,j.command_id,j.account_id,
              j.server_instance_id,j.credential_purpose,j.credential_object_id,
              j.credential_reference_id,j.credential_reference_key_id;
END;
$$;

REVOKE ALL ON FUNCTION app.claim_unfinished_credential_cleanup(text,timestamptz,timestamptz)
    FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker;
GRANT EXECUTE ON FUNCTION app.claim_unfinished_credential_cleanup(text,timestamptz,timestamptz)
    TO baby_tracker_apple_revoker;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
SELECT 8,principal,object_kind,object_identity,privileges
  FROM public.schema_privilege_manifest WHERE schema_version=7;
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges) VALUES
    (8,'baby_tracker_apple_revoker','function','app.claim_unfinished_credential_cleanup(text,timestamp with time zone,timestamp with time zone)','EXECUTE');

UPDATE public.schema_compatibility
   SET current_version=8,introduced_by='000008_lifecycle_worker_resume.sql',recorded_at=now()
 WHERE singleton;
