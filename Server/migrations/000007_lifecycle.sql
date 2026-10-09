-- Lifecycle/recovery projection and the least-privilege credential maintenance
-- surface. This migration is immutable once released.

ALTER TABLE public.schema_privilege_manifest DROP CONSTRAINT schema_privilege_manifest_principal_check;
ALTER TABLE public.schema_privilege_manifest ADD CONSTRAINT schema_privilege_manifest_principal_check
    CHECK(principal IN ('baby_tracker_runtime','baby_tracker_projector','baby_tracker_apple_revoker'));

ALTER TABLE public.account_security_state
    ADD COLUMN lifecycle_generation bigint;
UPDATE public.account_security_state SET lifecycle_generation=1;
ALTER TABLE public.account_security_state
    ALTER COLUMN lifecycle_generation SET NOT NULL,
    ADD CONSTRAINT account_security_lifecycle_generation_valid
        CHECK (lifecycle_generation BETWEEN 1 AND 9007199254740991);
ALTER TABLE public.account_security_state DROP CONSTRAINT account_security_state_identity_key_key;
ALTER TABLE public.account_security_state
    ADD CONSTRAINT account_security_identity_generation_unique UNIQUE(identity_key,lifecycle_generation);
CREATE UNIQUE INDEX account_security_one_live_identity_idx
    ON public.account_security_state(identity_key) WHERE status<>'deleted';

CREATE FUNCTION app.assign_actor_lifecycle_generation() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog
AS $$
BEGIN
    IF NEW.lifecycle_generation IS NULL THEN
        SELECT coalesce(max(lifecycle_generation),0)+1 INTO NEW.lifecycle_generation
          FROM public.account_security_state WHERE identity_key=NEW.identity_key;
    END IF;
    RETURN NEW;
END;
$$;
REVOKE ALL ON FUNCTION app.assign_actor_lifecycle_generation() FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker;
CREATE TRIGGER account_security_assign_lifecycle_generation
    BEFORE INSERT ON public.account_security_state
    FOR EACH ROW EXECUTE FUNCTION app.assign_actor_lifecycle_generation();

CREATE TABLE public.family_recovery_claims (
    family_id uuid PRIMARY KEY REFERENCES public.families(id),
    account_id uuid NOT NULL REFERENCES public.accounts(id),
    purge_after timestamptz NOT NULL CHECK (isfinite(purge_after)),
    status text NOT NULL CHECK(status IN ('active','recovered','forfeited','expired')),
    revision bigint NOT NULL CHECK(revision BETWEEN 1 AND 9007199254740991)
);
CREATE UNIQUE INDEX family_recovery_claims_one_active_per_account_idx
    ON public.family_recovery_claims(account_id) WHERE status='active';
ALTER TABLE public.family_recovery_claims ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.family_recovery_claims FORCE ROW LEVEL SECURITY;
CREATE POLICY family_recovery_context ON public.family_recovery_claims
    USING (family_id=app.current_family_id() OR account_id=app.current_account_id())
    WITH CHECK (family_id=app.current_family_id() OR account_id=app.current_account_id());

CREATE TABLE public.operator_audit_events (
    audit_id uuid PRIMARY KEY DEFAULT gen_random_uuid()
        CHECK (audit_id<>'00000000-0000-0000-0000-000000000000'),
    generation bigint NOT NULL CHECK(generation BETWEEN 1 AND 9007199254740991),
    revision bigint NOT NULL CHECK(revision BETWEEN 1 AND 9007199254740991),
    command_id uuid NOT NULL CHECK(command_id<>'00000000-0000-0000-0000-000000000000'),
    operator_id uuid NOT NULL CHECK(operator_id<>'00000000-0000-0000-0000-000000000000'),
    family_id uuid REFERENCES public.families(id),
    account_id uuid REFERENCES public.accounts(id),
    action text NOT NULL CHECK(action IN ('owner_recovered','operator_entitlement_granted','operator_entitlement_suspended')),
    policy_id text NOT NULL CHECK(policy_id ~ '^[A-Za-z][A-Za-z0-9._-]{0,127}$'),
    evidence_digest bytea NOT NULL CHECK(octet_length(evidence_digest)=32),
    reason_code text NOT NULL CHECK(reason_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    expected_revision bigint NOT NULL CHECK(expected_revision BETWEEN 1 AND 9007199254740991),
    occurred_at timestamptz NOT NULL CHECK(isfinite(occurred_at)),
    created_at timestamptz NOT NULL DEFAULT now() CHECK(isfinite(created_at)),
    UNIQUE(generation,revision)
);

CREATE FUNCTION app.lifecycle_operation_uuid(p_domain text,p_command_id uuid) RETURNS uuid
LANGUAGE sql IMMUTABLE STRICT
SET search_path=pg_catalog
AS $$
    SELECT (substr(value,1,8)||'-'||substr(value,9,4)||'-8'||substr(value,14,3)||'-8'||substr(value,18,3)||'-'||substr(value,21,12))::uuid
      FROM (SELECT md5(p_domain||':'||p_command_id::text) AS value) digest
$$;
REVOKE ALL ON FUNCTION app.lifecycle_operation_uuid(text,uuid)
    FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker;

DO $$
DECLARE constraint_name text;
BEGIN
    SELECT conname INTO STRICT constraint_name FROM pg_constraint
     WHERE conrelid='public.apple_revocation_jobs'::regclass AND contype='c'
       AND pg_get_constraintdef(oid) LIKE '%status%pending%retry%complete%';
    EXECUTE format('ALTER TABLE public.apple_revocation_jobs DROP CONSTRAINT %I',constraint_name);
END
$$;
ALTER TABLE public.apple_revocation_jobs
    ADD COLUMN deletion_operation_id uuid,
    ADD COLUMN deletion_command_id uuid,
    ADD COLUMN operation_version bigint NOT NULL DEFAULT 1
        CHECK(operation_version BETWEEN 1 AND 9007199254740991),
    ADD COLUMN provider_revoked_at timestamptz CHECK(provider_revoked_at IS NULL OR isfinite(provider_revoked_at)),
    ADD COLUMN cleanup_started_at timestamptz CHECK(cleanup_started_at IS NULL OR isfinite(cleanup_started_at)),
    ADD COLUMN lease_owner text CHECK(lease_owner IS NULL OR (octet_length(lease_owner) BETWEEN 1 AND 128 AND octet_length(lease_owner)=char_length(lease_owner) AND lease_owner !~ '[^A-Za-z0-9._:-]')),
    ADD COLUMN lease_token text CHECK(lease_token IS NULL OR lease_token ~ '^[A-Za-z0-9._:-]{1,128}$'),
    ADD COLUMN lease_expires_at timestamptz CHECK(lease_expires_at IS NULL OR isfinite(lease_expires_at)),
    ADD CONSTRAINT apple_revocation_jobs_status_check
        CHECK(status IN ('pending','retry','revoke_succeeded','cleanup_pending','complete')),
    ADD CONSTRAINT apple_revocation_jobs_lease_complete_check
        CHECK ((lease_owner IS NULL)=(lease_token IS NULL) AND (lease_token IS NULL)=(lease_expires_at IS NULL));
UPDATE public.apple_revocation_jobs
   SET deletion_operation_id=app.lifecycle_operation_uuid('apple-delete-operation',job_id),
       deletion_command_id=app.lifecycle_operation_uuid('apple-delete-command',job_id);
ALTER TABLE public.apple_revocation_jobs
    ALTER COLUMN deletion_operation_id SET NOT NULL,
    ALTER COLUMN deletion_command_id SET NOT NULL,
    ADD CONSTRAINT apple_revocation_operation_nonzero CHECK(deletion_operation_id<>'00000000-0000-0000-0000-000000000000'),
    ADD CONSTRAINT apple_revocation_command_nonzero CHECK(deletion_command_id<>'00000000-0000-0000-0000-000000000000');

CREATE TABLE public.credential_cleanup_jobs (
    account_id uuid NOT NULL CHECK(account_id<>'00000000-0000-0000-0000-000000000000'),
    server_instance_id uuid NOT NULL CHECK(server_instance_id<>'00000000-0000-0000-0000-000000000000'),
    credential_purpose text NOT NULL CHECK(credential_purpose='apple-refresh-revocation-v1'),
    credential_object_id uuid NOT NULL CHECK(credential_object_id<>'00000000-0000-0000-0000-000000000000'),
    credential_reference_id text NOT NULL CHECK(credential_reference_id ~ '^[A-Za-z0-9._-]{1,128}$'),
    credential_reference_key_id text NOT NULL CHECK(credential_reference_key_id ~ '^[A-Za-z0-9._-]{1,128}$'),
    operation_id uuid NOT NULL CHECK(operation_id<>'00000000-0000-0000-0000-000000000000'),
    command_id uuid NOT NULL CHECK(command_id<>'00000000-0000-0000-0000-000000000000'),
    operation_version bigint NOT NULL DEFAULT 1 CHECK(operation_version BETWEEN 1 AND 9007199254740991),
    status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','retry','complete')),
    attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 1000000),
    next_attempt_at timestamptz NOT NULL CHECK(isfinite(next_attempt_at)),
    lease_owner text CHECK(lease_owner IS NULL OR (octet_length(lease_owner) BETWEEN 1 AND 128 AND octet_length(lease_owner)=char_length(lease_owner) AND lease_owner !~ '[^A-Za-z0-9._:-]')),
    lease_token text CHECK(lease_token IS NULL OR lease_token ~ '^[A-Za-z0-9._:-]{1,128}$'),
    lease_expires_at timestamptz CHECK(lease_expires_at IS NULL OR isfinite(lease_expires_at)),
    last_error_code text CHECK(last_error_code IS NULL OR last_error_code ~ '^[a-z0-9_]{1,64}$'),
    created_at timestamptz NOT NULL CHECK(isfinite(created_at)),
    updated_at timestamptz NOT NULL CHECK(isfinite(updated_at)),
    completed_at timestamptz CHECK(completed_at IS NULL OR isfinite(completed_at)),
    PRIMARY KEY(account_id,credential_object_id,credential_reference_id,credential_reference_key_id),
    CHECK ((lease_owner IS NULL)=(lease_token IS NULL) AND (lease_token IS NULL)=(lease_expires_at IS NULL))
);
CREATE INDEX credential_cleanup_jobs_due_idx ON public.credential_cleanup_jobs(status,next_attempt_at);

REVOKE ALL ON TABLE public.family_recovery_claims,public.operator_audit_events,
    public.credential_cleanup_jobs,public.apple_revocation_jobs
    FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker;

CREATE FUNCTION app.assert_apple_revocation_claim(
    p_job_id uuid,p_lease_token text,p_operation_version text,p_operation_id uuid,p_command_id uuid,
    p_status text,p_account_id uuid,p_server_instance_id uuid,p_purpose text,p_object_id uuid,
    p_reference_id text,p_reference_key_id text,p_attempts integer,p_lease_expires_at timestamptz,p_now timestamptz
) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE matched boolean;
BEGIN
    IF p_operation_version !~ '^[1-9][0-9]*$' OR length(p_operation_version)>16
       OR p_lease_token !~ '^[A-Za-z0-9._:-]{1,128}$' OR p_now IS NULL OR NOT isfinite(p_now) THEN
        RAISE EXCEPTION 'stale lease' USING ERRCODE='BT003';
    END IF;
    SELECT true INTO matched FROM public.apple_revocation_jobs j WHERE
        j.job_id=p_job_id AND j.lease_token=p_lease_token
        AND j.operation_version=p_operation_version::bigint
        AND j.deletion_operation_id=p_operation_id AND j.deletion_command_id=p_command_id
        AND j.status=p_status AND j.account_id=p_account_id AND j.server_instance_id=p_server_instance_id
        AND j.credential_purpose=p_purpose AND j.credential_object_id=p_object_id
        AND j.credential_reference_id=p_reference_id AND j.credential_reference_key_id=p_reference_key_id
        AND j.attempts=p_attempts AND j.lease_expires_at=p_lease_expires_at
        AND p_now<j.lease_expires_at FOR UPDATE;
    IF NOT coalesce(matched,false) THEN RAISE EXCEPTION 'stale lease' USING ERRCODE='BT003'; END IF;
END;
$$;

CREATE FUNCTION app.claim_apple_revocation_job(p_worker_id text,p_now timestamptz,p_lease_expires_at timestamptz)
RETURNS TABLE(job_id uuid,lease_token text,operation_version text,operation_id uuid,command_id uuid,status text,
    account_id uuid,server_instance_id uuid,purpose text,object_id uuid,reference_id text,reference_key_id text,
    attempts integer,lease_expires_at timestamptz)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE selected uuid; token text;
BEGIN
    IF p_worker_id !~ '^[A-Za-z0-9._:-]{1,128}$' OR p_now IS NULL OR NOT isfinite(p_now)
       OR p_lease_expires_at IS NULL OR NOT isfinite(p_lease_expires_at)
       OR p_lease_expires_at<=p_now OR p_lease_expires_at>p_now+interval '5 minutes' THEN
        RAISE EXCEPTION 'invalid claim' USING ERRCODE='22023';
    END IF;
    SELECT j.job_id INTO selected FROM public.apple_revocation_jobs j
     WHERE j.status<>'complete' AND j.next_attempt_at<=p_now
       AND (j.lease_expires_at IS NULL OR j.lease_expires_at<=p_now)
     ORDER BY j.next_attempt_at,j.created_at,j.job_id FOR UPDATE SKIP LOCKED LIMIT 1;
    IF selected IS NULL THEN RETURN; END IF;
    token:=encode(sha256(convert_to(gen_random_uuid()::text||clock_timestamp()::text||p_worker_id,'UTF8')),'hex');
    RETURN QUERY UPDATE public.apple_revocation_jobs j SET lease_owner=p_worker_id,lease_token=token,
        lease_expires_at=p_lease_expires_at,attempts=j.attempts+1,operation_version=j.operation_version+1,updated_at=p_now
      WHERE j.job_id=selected
      RETURNING j.job_id,j.lease_token,j.operation_version::text,j.deletion_operation_id,j.deletion_command_id,j.status,
        j.account_id,j.server_instance_id,j.credential_purpose,j.credential_object_id,j.credential_reference_id,
        j.credential_reference_key_id,j.attempts,j.lease_expires_at;
END;
$$;

CREATE FUNCTION app.validate_apple_revocation_claim(
    p_job_id uuid,p_lease_token text,p_operation_version text,p_operation_id uuid,p_command_id uuid,p_status text,
    p_account_id uuid,p_server_instance_id uuid,p_purpose text,p_object_id uuid,p_reference_id text,p_reference_key_id text,
    p_attempts integer,p_lease_expires_at timestamptz,p_now timestamptz) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$
 SELECT app.assert_apple_revocation_claim($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
$$;

CREATE FUNCTION app.apple_credential_referenced_outside_job(
    p_job_id uuid,p_lease_token text,p_operation_version text,p_operation_id uuid,p_command_id uuid,p_status text,
    p_account_id uuid,p_server_instance_id uuid,p_purpose text,p_object_id uuid,p_reference_id text,p_reference_key_id text,
    p_attempts integer,p_lease_expires_at timestamptz,p_now timestamptz) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE referenced boolean;
BEGIN
    PERFORM app.assert_apple_revocation_claim(p_job_id,p_lease_token,p_operation_version,p_operation_id,p_command_id,p_status,
        p_account_id,p_server_instance_id,p_purpose,p_object_id,p_reference_id,p_reference_key_id,p_attempts,p_lease_expires_at,p_now);
    SELECT EXISTS(SELECT FROM public.account_identities i WHERE i.account_id=p_account_id
        AND i.apple_credential_object_id=p_object_id AND i.apple_credential_id=p_reference_id AND i.apple_credential_key_id=p_reference_key_id)
      OR EXISTS(SELECT FROM public.auth_exchange_material m WHERE m.projected_at IS NULL AND m.expires_at>=p_now
        AND m.account_id=p_account_id AND m.apple_credential_object_id=p_object_id AND m.apple_credential_id=p_reference_id
        AND m.apple_credential_key_id=p_reference_key_id)
      OR EXISTS(SELECT FROM public.apple_revocation_jobs j WHERE j.job_id<>p_job_id
        AND j.account_id=p_account_id AND j.credential_object_id=p_object_id
        AND j.credential_reference_id=p_reference_id AND j.credential_reference_key_id=p_reference_key_id)
      INTO referenced;
    RETURN referenced;
END;
$$;

CREATE FUNCTION app.mark_apple_provider_revoked(
    p_job_id uuid,p_lease_token text,p_operation_version text,p_operation_id uuid,p_command_id uuid,p_status text,
    p_account_id uuid,p_server_instance_id uuid,p_purpose text,p_object_id uuid,p_reference_id text,p_reference_key_id text,
    p_attempts integer,p_lease_expires_at timestamptz,p_now timestamptz) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    PERFORM app.assert_apple_revocation_claim(p_job_id,p_lease_token,p_operation_version,p_operation_id,p_command_id,p_status,
        p_account_id,p_server_instance_id,p_purpose,p_object_id,p_reference_id,p_reference_key_id,p_attempts,p_lease_expires_at,p_now);
    IF p_status NOT IN ('pending','retry') THEN RAISE EXCEPTION 'stale lease' USING ERRCODE='BT003'; END IF;
    UPDATE public.apple_revocation_jobs SET status='revoke_succeeded',provider_revoked_at=p_now,updated_at=p_now,
        lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,last_error_code=NULL WHERE job_id=p_job_id;
END;
$$;

CREATE FUNCTION app.begin_apple_credential_cleanup(
    p_job_id uuid,p_lease_token text,p_operation_version text,p_operation_id uuid,p_command_id uuid,p_status text,
    p_account_id uuid,p_server_instance_id uuid,p_purpose text,p_object_id uuid,p_reference_id text,p_reference_key_id text,
    p_attempts integer,p_lease_expires_at timestamptz,p_now timestamptz) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    PERFORM app.assert_apple_revocation_claim(p_job_id,p_lease_token,p_operation_version,p_operation_id,p_command_id,p_status,
        p_account_id,p_server_instance_id,p_purpose,p_object_id,p_reference_id,p_reference_key_id,p_attempts,p_lease_expires_at,p_now);
    IF p_status<>'revoke_succeeded' THEN RAISE EXCEPTION 'stale lease' USING ERRCODE='BT003'; END IF;
    UPDATE public.apple_revocation_jobs SET status='cleanup_pending',cleanup_started_at=p_now,updated_at=p_now,
        lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,last_error_code=NULL WHERE job_id=p_job_id;
END;
$$;

CREATE FUNCTION app.complete_apple_revocation_job(
    p_job_id uuid,p_lease_token text,p_operation_version text,p_operation_id uuid,p_command_id uuid,p_status text,
    p_account_id uuid,p_server_instance_id uuid,p_purpose text,p_object_id uuid,p_reference_id text,p_reference_key_id text,
    p_attempts integer,p_lease_expires_at timestamptz,p_now timestamptz) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    PERFORM app.assert_apple_revocation_claim(p_job_id,p_lease_token,p_operation_version,p_operation_id,p_command_id,p_status,
        p_account_id,p_server_instance_id,p_purpose,p_object_id,p_reference_id,p_reference_key_id,p_attempts,p_lease_expires_at,p_now);
    IF p_status<>'cleanup_pending' THEN RAISE EXCEPTION 'stale lease' USING ERRCODE='BT003'; END IF;
    UPDATE public.apple_revocation_jobs SET status='complete',completed_at=p_now,updated_at=p_now,
        lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,last_error_code=NULL WHERE job_id=p_job_id;
END;
$$;

CREATE FUNCTION app.retry_apple_revocation_job(
    p_job_id uuid,p_lease_token text,p_operation_version text,p_operation_id uuid,p_command_id uuid,p_status text,
    p_account_id uuid,p_server_instance_id uuid,p_purpose text,p_object_id uuid,p_reference_id text,p_reference_key_id text,
    p_attempts integer,p_lease_expires_at timestamptz,p_error_code text,p_next_attempt_at timestamptz,p_now timestamptz) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    PERFORM app.assert_apple_revocation_claim(p_job_id,p_lease_token,p_operation_version,p_operation_id,p_command_id,p_status,
        p_account_id,p_server_instance_id,p_purpose,p_object_id,p_reference_id,p_reference_key_id,p_attempts,p_lease_expires_at,p_now);
    IF p_error_code !~ '^[a-z0-9_]{1,64}$' OR p_next_attempt_at<=p_now OR NOT isfinite(p_next_attempt_at) THEN
        RAISE EXCEPTION 'invalid retry' USING ERRCODE='22023';
    END IF;
    UPDATE public.apple_revocation_jobs SET status=CASE WHEN status IN ('pending','retry') THEN 'retry' ELSE status END,
        next_attempt_at=p_next_attempt_at,last_error_code=p_error_code,updated_at=p_now,
        lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL WHERE job_id=p_job_id;
END;
$$;

CREATE FUNCTION app.assert_credential_cleanup_claim(
    p_lease_token text,p_operation_version text,p_operation_id uuid,p_command_id uuid,p_account_id uuid,
    p_object_id uuid,p_reference_id text,p_reference_key_id text,p_now timestamptz) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE matched boolean;
BEGIN
    IF p_operation_version !~ '^[1-9][0-9]*$' OR length(p_operation_version)>16
       OR p_lease_token !~ '^[A-Za-z0-9._:-]{1,128}$' OR p_now IS NULL OR NOT isfinite(p_now) THEN
        RAISE EXCEPTION 'stale lease' USING ERRCODE='BT003';
    END IF;
    SELECT true INTO matched FROM public.credential_cleanup_jobs j WHERE j.lease_token=p_lease_token
        AND j.operation_version=p_operation_version::bigint AND j.operation_id=p_operation_id AND j.command_id=p_command_id
        AND j.account_id=p_account_id AND j.credential_object_id=p_object_id
        AND j.credential_reference_id=p_reference_id AND j.credential_reference_key_id=p_reference_key_id
        AND p_now<j.lease_expires_at FOR UPDATE;
    IF NOT coalesce(matched,false) THEN RAISE EXCEPTION 'stale lease' USING ERRCODE='BT003'; END IF;
END;
$$;

CREATE FUNCTION app.claim_credential_cleanup(p_account_id uuid,p_server_instance_id uuid,p_purpose text,p_object_id uuid,
    p_reference_id text,p_reference_key_id text,p_worker_id text,p_now timestamptz,p_lease_expires_at timestamptz)
RETURNS TABLE(lease_token text,operation_version text,operation_id uuid,command_id uuid,account_id uuid,object_id uuid,reference_id text,reference_key_id text)
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE token text;
BEGIN
    IF p_account_id IS NULL OR p_account_id='00000000-0000-0000-0000-000000000000' OR p_server_instance_id IS NULL
       OR p_server_instance_id='00000000-0000-0000-0000-000000000000' OR p_purpose<>'apple-refresh-revocation-v1'
       OR p_object_id IS NULL OR p_object_id='00000000-0000-0000-0000-000000000000'
       OR p_reference_id !~ '^[A-Za-z0-9._-]{1,128}$' OR p_reference_key_id !~ '^[A-Za-z0-9._-]{1,128}$'
       OR p_worker_id !~ '^[A-Za-z0-9._:-]{1,128}$' OR p_lease_expires_at<=p_now
       OR p_lease_expires_at>p_now+interval '5 minutes' OR NOT isfinite(p_now) OR NOT isfinite(p_lease_expires_at) THEN
        RAISE EXCEPTION 'invalid cleanup claim' USING ERRCODE='22023';
    END IF;
    IF EXISTS(SELECT FROM public.account_identities i WHERE i.account_id=p_account_id AND i.apple_credential_object_id=p_object_id
        AND i.apple_credential_id=p_reference_id AND i.apple_credential_key_id=p_reference_key_id)
       OR EXISTS(SELECT FROM public.auth_exchange_material m WHERE m.projected_at IS NULL AND m.expires_at>=p_now
        AND m.account_id=p_account_id AND m.apple_credential_object_id=p_object_id AND m.apple_credential_id=p_reference_id
        AND m.apple_credential_key_id=p_reference_key_id)
       OR EXISTS(SELECT FROM public.apple_revocation_jobs j WHERE j.account_id=p_account_id
        AND j.credential_object_id=p_object_id AND j.credential_reference_id=p_reference_id
        AND j.credential_reference_key_id=p_reference_key_id) THEN RETURN; END IF;
    INSERT INTO public.credential_cleanup_jobs(account_id,server_instance_id,credential_purpose,credential_object_id,
        credential_reference_id,credential_reference_key_id,operation_id,command_id,next_attempt_at,created_at,updated_at)
    VALUES(p_account_id,p_server_instance_id,p_purpose,p_object_id,p_reference_id,p_reference_key_id,
        gen_random_uuid(),gen_random_uuid(),p_now,p_now,p_now)
    ON CONFLICT ON CONSTRAINT credential_cleanup_jobs_pkey DO UPDATE SET
        server_instance_id=excluded.server_instance_id,credential_purpose=excluded.credential_purpose,
        operation_id=gen_random_uuid(),command_id=gen_random_uuid(),
        operation_version=credential_cleanup_jobs.operation_version+1,status='pending',attempts=0,
        next_attempt_at=p_now,lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,last_error_code=NULL,
        updated_at=p_now,completed_at=NULL
      WHERE credential_cleanup_jobs.status='complete';
    token:=encode(sha256(convert_to(gen_random_uuid()::text||clock_timestamp()::text||p_worker_id,'UTF8')),'hex');
    RETURN QUERY UPDATE public.credential_cleanup_jobs j SET lease_owner=p_worker_id,lease_token=token,
        lease_expires_at=p_lease_expires_at,attempts=j.attempts+1,operation_version=j.operation_version+1,updated_at=p_now
      WHERE j.account_id=p_account_id AND j.credential_object_id=p_object_id
        AND j.credential_reference_id=p_reference_id AND j.credential_reference_key_id=p_reference_key_id
        AND j.status<>'complete' AND j.next_attempt_at<=p_now AND (j.lease_expires_at IS NULL OR j.lease_expires_at<=p_now)
      RETURNING j.lease_token,j.operation_version::text,j.operation_id,j.command_id,j.account_id,j.credential_object_id,
        j.credential_reference_id,j.credential_reference_key_id;
END;
$$;

CREATE FUNCTION app.validate_credential_cleanup_claim(p_lease_token text,p_operation_version text,p_operation_id uuid,p_command_id uuid,
    p_account_id uuid,p_object_id uuid,p_reference_id text,p_reference_key_id text,p_now timestamptz) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog AS $$ SELECT app.assert_credential_cleanup_claim($1,$2,$3,$4,$5,$6,$7,$8,$9) $$;

CREATE FUNCTION app.credential_cleanup_referenced(p_lease_token text,p_operation_version text,p_operation_id uuid,p_command_id uuid,
    p_account_id uuid,p_object_id uuid,p_reference_id text,p_reference_key_id text,p_now timestamptz) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE referenced boolean;
BEGIN
    PERFORM app.assert_credential_cleanup_claim(p_lease_token,p_operation_version,p_operation_id,p_command_id,p_account_id,p_object_id,p_reference_id,p_reference_key_id,p_now);
    SELECT EXISTS(SELECT FROM public.account_identities i WHERE i.account_id=p_account_id AND i.apple_credential_object_id=p_object_id
        AND i.apple_credential_id=p_reference_id AND i.apple_credential_key_id=p_reference_key_id)
      OR EXISTS(SELECT FROM public.auth_exchange_material m WHERE m.projected_at IS NULL AND m.expires_at>=p_now
        AND m.account_id=p_account_id AND m.apple_credential_object_id=p_object_id AND m.apple_credential_id=p_reference_id
        AND m.apple_credential_key_id=p_reference_key_id)
      OR EXISTS(SELECT FROM public.apple_revocation_jobs j WHERE j.account_id=p_account_id
        AND j.credential_object_id=p_object_id AND j.credential_reference_id=p_reference_id
        AND j.credential_reference_key_id=p_reference_key_id) INTO referenced;
    RETURN referenced;
END;
$$;

CREATE FUNCTION app.finish_credential_cleanup(p_lease_token text,p_operation_version text,p_operation_id uuid,p_command_id uuid,
    p_account_id uuid,p_object_id uuid,p_reference_id text,p_reference_key_id text,p_now timestamptz) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    PERFORM app.assert_credential_cleanup_claim(p_lease_token,p_operation_version,p_operation_id,p_command_id,p_account_id,p_object_id,p_reference_id,p_reference_key_id,p_now);
    UPDATE public.credential_cleanup_jobs SET status='complete',completed_at=p_now,updated_at=p_now,last_error_code=NULL,
        lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL WHERE account_id=p_account_id AND credential_object_id=p_object_id
        AND credential_reference_id=p_reference_id AND credential_reference_key_id=p_reference_key_id;
END;
$$;

CREATE FUNCTION app.retry_credential_cleanup(p_lease_token text,p_operation_version text,p_operation_id uuid,p_command_id uuid,
    p_account_id uuid,p_object_id uuid,p_reference_id text,p_reference_key_id text,p_error_code text,
    p_next_attempt_at timestamptz,p_now timestamptz) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
BEGIN
    PERFORM app.assert_credential_cleanup_claim(p_lease_token,p_operation_version,p_operation_id,p_command_id,p_account_id,p_object_id,p_reference_id,p_reference_key_id,p_now);
    IF p_error_code !~ '^[a-z0-9_]{1,64}$' OR p_next_attempt_at<=p_now OR NOT isfinite(p_next_attempt_at) THEN
        RAISE EXCEPTION 'invalid retry' USING ERRCODE='22023'; END IF;
    UPDATE public.credential_cleanup_jobs SET status='retry',next_attempt_at=p_next_attempt_at,last_error_code=p_error_code,
        updated_at=p_now,lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL
      WHERE account_id=p_account_id AND credential_object_id=p_object_id
        AND credential_reference_id=p_reference_id AND credential_reference_key_id=p_reference_key_id;
END;
$$;

REVOKE ALL ON FUNCTION app.assert_apple_revocation_claim(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
    app.assert_credential_cleanup_claim(text,text,uuid,uuid,uuid,uuid,text,text,timestamptz)
    FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker;

REVOKE ALL ON FUNCTION app.claim_apple_revocation_job(text,timestamptz,timestamptz),
    app.validate_apple_revocation_claim(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
    app.apple_credential_referenced_outside_job(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
    app.mark_apple_provider_revoked(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
    app.begin_apple_credential_cleanup(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
    app.complete_apple_revocation_job(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
    app.retry_apple_revocation_job(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,text,timestamptz,timestamptz),
    app.claim_credential_cleanup(uuid,uuid,text,uuid,text,text,text,timestamptz,timestamptz),
    app.validate_credential_cleanup_claim(text,text,uuid,uuid,uuid,uuid,text,text,timestamptz),
    app.credential_cleanup_referenced(text,text,uuid,uuid,uuid,uuid,text,text,timestamptz),
    app.finish_credential_cleanup(text,text,uuid,uuid,uuid,uuid,text,text,timestamptz),
    app.retry_credential_cleanup(text,text,uuid,uuid,uuid,uuid,text,text,text,timestamptz,timestamptz)
    FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker;
GRANT USAGE ON SCHEMA app TO baby_tracker_apple_revoker;
GRANT EXECUTE ON FUNCTION app.claim_apple_revocation_job(text,timestamptz,timestamptz),
    app.validate_apple_revocation_claim(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
    app.apple_credential_referenced_outside_job(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
    app.mark_apple_provider_revoked(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
    app.begin_apple_credential_cleanup(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
    app.complete_apple_revocation_job(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,timestamptz),
    app.retry_apple_revocation_job(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamptz,text,timestamptz,timestamptz),
    app.claim_credential_cleanup(uuid,uuid,text,uuid,text,text,text,timestamptz,timestamptz),
    app.validate_credential_cleanup_claim(text,text,uuid,uuid,uuid,uuid,text,text,timestamptz),
    app.credential_cleanup_referenced(text,text,uuid,uuid,uuid,uuid,text,text,timestamptz),
    app.finish_credential_cleanup(text,text,uuid,uuid,uuid,uuid,text,text,timestamptz),
    app.retry_credential_cleanup(text,text,uuid,uuid,uuid,uuid,text,text,text,timestamptz,timestamptz)
    TO baby_tracker_apple_revoker;

ALTER FUNCTION app.project_security_event(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)
    RENAME TO project_security_event_v6;
REVOKE ALL ON FUNCTION app.project_security_event_v6(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)
    FROM PUBLIC,baby_tracker_runtime,baby_tracker_projector,baby_tracker_apple_revoker;

CREATE FUNCTION app.project_security_event(
    p_generation bigint,p_revision bigint,p_previous_hash bytea,p_event_hash bytea,
    p_actor_account_id uuid,p_command_id uuid,p_command_hash bytea,p_result jsonb,p_event jsonb,p_event_bytes bytea
) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog AS $$
DECLARE
    event_kind text;
    event_at timestamptz;
    effect jsonb;
    canonical_effect text;
    canonical_effects text:='[';
    canonical_event text;
    first_effect boolean:=true;
    current_generation bigint;
    current_revision bigint;
    current_hash bytea;
    affected_rows bigint;
    anchor jsonb;
    old_owner jsonb;
    new_owner jsonb;
    family_value uuid;
    account_value uuid;
    previous_account text:=coalesce(current_setting('app.account_id',true),'');
    previous_family text:=coalesce(current_setting('app.family_id',true),'');
    identity_record record;
    server_value uuid;
BEGIN
    event_kind:=p_event->>'kind';
    IF event_kind NOT IN ('family_deletion_scheduled','family_recovered','recovery_forfeited','account_deleted',
        'owner_recovered','operator_entitlement_granted','operator_entitlement_suspended') THEN
        PERFORM app.project_security_event_v6(p_generation,p_revision,p_previous_hash,p_event_hash,p_actor_account_id,
            p_command_id,p_command_hash,p_result,p_event,p_event_bytes);
        RETURN;
    END IF;
    IF p_generation IS NULL OR p_revision IS NULL OR p_generation<=0 OR p_revision<=0
       OR p_generation>9007199254740991 OR p_revision>9007199254740991
       OR octet_length(p_previous_hash) IS DISTINCT FROM 32 OR octet_length(p_event_hash) IS DISTINCT FROM 32
       OR octet_length(p_command_hash) IS DISTINCT FROM 32 OR octet_length(p_event_bytes) NOT BETWEEN 1 AND 65536
       OR p_actor_account_id IS NULL OR p_actor_account_id='00000000-0000-0000-0000-000000000000'
       OR p_command_id IS NULL OR p_command_id='00000000-0000-0000-0000-000000000000'
       OR sha256(p_event_bytes) IS DISTINCT FROM p_event_hash
       OR convert_from(p_event_bytes,'UTF8')::jsonb IS DISTINCT FROM p_event
       OR jsonb_typeof(p_event) IS DISTINCT FROM 'object'
       OR (p_event-ARRAY['version','generation','revision','previous_hash','command_id','command_hash','actor_account_id','actor_device_id','kind','at','effects']::text[])<>'{}'::jsonb
       OR NOT (p_event ?& ARRAY['version','generation','revision','previous_hash','command_id','command_hash','actor_account_id','actor_device_id','kind','at','effects'])
       OR p_event->>'version'<>'v1' OR jsonb_typeof(p_event->'generation')<>'number' OR jsonb_typeof(p_event->'revision')<>'number'
       OR (p_event->>'generation')::bigint<>p_generation OR (p_event->>'revision')::bigint<>p_revision
       OR p_event->>'previous_hash' !~ '^[0-9a-f]{64}$' OR decode(p_event->>'previous_hash','hex')<>p_previous_hash
       OR p_event->>'command_hash' !~ '^[0-9a-f]{64}$' OR decode(p_event->>'command_hash','hex')<>p_command_hash
       OR p_event->>'actor_account_id'<>p_actor_account_id::text OR (p_event->>'actor_account_id')::uuid<>p_actor_account_id
       OR p_event->>'command_id'<>p_command_id::text OR (p_event->>'command_id')::uuid<>p_command_id
       OR jsonb_typeof(p_event->'actor_device_id')<>'string'
       OR (p_event->>'actor_device_id'<>'' AND (p_event->>'actor_device_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
           OR p_event->>'actor_device_id'='00000000-0000-0000-0000-000000000000'))
       OR jsonb_typeof(p_event->'at')<>'string'
       OR p_event->>'at' !~ '^[0-9]{4}-(0[1-9]|1[0-2])-([0-2][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{0,8}[1-9])?Z$'
       OR jsonb_typeof(p_event->'effects')<>'array' OR jsonb_array_length(p_event->'effects')=0
       OR p_result IS DISTINCT FROM jsonb_build_object('command_id',p_command_id::text,'outcome','committed','code','ok','revision',p_revision) THEN
        RAISE EXCEPTION 'invalid lifecycle event envelope' USING ERRCODE='22023';
    END IF;
    event_at:=(p_event->>'at')::timestamptz;
    IF event_at IS NULL OR NOT isfinite(event_at) THEN RAISE EXCEPTION 'invalid lifecycle event time' USING ERRCODE='22023'; END IF;

    FOR effect IN SELECT value FROM jsonb_array_elements(p_event->'effects') LOOP
        IF jsonb_typeof(effect)<>'object' OR effect->>'kind'<>event_kind THEN
            RAISE EXCEPTION 'lifecycle effect kind mismatch' USING ERRCODE='22023';
        END IF;
        IF effect ? 'account_id' AND (effect->>'account_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
            OR effect->>'account_id'='00000000-0000-0000-0000-000000000000') THEN
            RAISE EXCEPTION 'invalid lifecycle account' USING ERRCODE='22023'; END IF;
        IF effect ? 'family_id' AND (effect->>'family_id' !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
            OR effect->>'family_id'='00000000-0000-0000-0000-000000000000') THEN
            RAISE EXCEPTION 'invalid lifecycle family' USING ERRCODE='22023'; END IF;
        CASE event_kind
        WHEN 'family_deletion_scheduled' THEN
            IF (effect-ARRAY['kind','account_id','family_id','role','status','purge_after']::text[])<>'{}'::jsonb
               OR NOT(effect ?& ARRAY['kind','account_id','family_id','role','status','purge_after'])
               OR effect->>'role' NOT IN ('owner','caregiver') OR effect->>'status' NOT IN ('deletion_pending','former')
               OR (effect->>'status'='deletion_pending' AND effect->>'role'<>'owner')
               OR (effect->>'purge_after')::timestamptz<>event_at+interval '30 days' THEN
                RAISE EXCEPTION 'invalid family deletion effect' USING ERRCODE='22023'; END IF;
            IF anchor IS NULL AND effect->>'status'='deletion_pending' THEN anchor:=effect; END IF;
            canonical_effect:='{"kind":'||app.go_json_string(event_kind)||',"account_id":'||app.go_json_string(effect->>'account_id')||
                ',"family_id":'||app.go_json_string(effect->>'family_id')||',"role":'||app.go_json_string(effect->>'role')||
                ',"status":'||app.go_json_string(effect->>'status')||',"purge_after":'||app.go_json_string(effect->>'purge_after')||'}';
        WHEN 'family_recovered' THEN
            IF jsonb_array_length(p_event->'effects')<>1 OR (effect-ARRAY['kind','account_id','family_id','role','status']::text[])<>'{}'::jsonb
               OR NOT(effect ?& ARRAY['kind','account_id','family_id','role','status']) OR effect->>'role'<>'owner' OR effect->>'status'<>'active' THEN
                RAISE EXCEPTION 'invalid family recovery effect' USING ERRCODE='22023'; END IF;
            canonical_effect:='{"kind":'||app.go_json_string(event_kind)||',"account_id":'||app.go_json_string(effect->>'account_id')||
                ',"family_id":'||app.go_json_string(effect->>'family_id')||',"role":'||app.go_json_string(effect->>'role')||
                ',"status":'||app.go_json_string(effect->>'status')||'}';
        WHEN 'recovery_forfeited' THEN
            IF jsonb_array_length(p_event->'effects')<>1 OR (effect-ARRAY['kind','account_id','family_id','status']::text[])<>'{}'::jsonb
               OR NOT(effect ?& ARRAY['kind','account_id','family_id','status']) OR effect->>'status'<>'forfeited' THEN
                RAISE EXCEPTION 'invalid recovery forfeiture effect' USING ERRCODE='22023'; END IF;
            canonical_effect:='{"kind":'||app.go_json_string(event_kind)||',"account_id":'||app.go_json_string(effect->>'account_id')||
                ',"family_id":'||app.go_json_string(effect->>'family_id')||',"status":'||app.go_json_string(effect->>'status')||'}';
        WHEN 'account_deleted' THEN
            IF jsonb_array_length(p_event->'effects')<>1 OR (effect-ARRAY['kind','account_id','identity_key','status']::text[])<>'{}'::jsonb
               OR NOT(effect ?& ARRAY['kind','account_id','identity_key','status']) OR effect->>'status'<>'deleted'
               OR effect->>'account_id'<>p_actor_account_id::text OR effect->>'identity_key'='' THEN
                RAISE EXCEPTION 'invalid account deletion effect' USING ERRCODE='22023'; END IF;
            canonical_effect:='{"kind":'||app.go_json_string(event_kind)||',"account_id":'||app.go_json_string(effect->>'account_id')||
                ',"identity_key":'||app.go_json_string(effect->>'identity_key')||',"status":'||app.go_json_string(effect->>'status')||'}';
        WHEN 'owner_recovered' THEN
            IF jsonb_array_length(p_event->'effects')<>2 OR (effect-ARRAY['kind','account_id','family_id','role','status','operator_policy_id','operator_evidence_digest','operator_reason_code','expected_revision']::text[])<>'{}'::jsonb
               OR NOT(effect ?& ARRAY['kind','account_id','family_id','role','status','operator_policy_id','operator_evidence_digest','operator_reason_code','expected_revision'])
               OR effect->>'role' NOT IN ('owner','caregiver') OR effect->>'status'<>'active'
               OR effect->>'operator_policy_id' !~ '^[A-Za-z][A-Za-z0-9._-]{0,127}$' OR effect->>'operator_evidence_digest' !~ '^[0-9a-f]{64}$'
               OR effect->>'operator_reason_code'<>'owner_identity_unavailable' OR jsonb_typeof(effect->'expected_revision')<>'number'
               OR (effect->>'expected_revision')::bigint<=0 OR (effect->>'expected_revision')::bigint<>p_revision-1 THEN
                RAISE EXCEPTION 'invalid owner recovery effect' USING ERRCODE='22023'; END IF;
            IF effect->>'role'='owner' THEN new_owner:=effect; ELSE old_owner:=effect; END IF;
            canonical_effect:='{"kind":'||app.go_json_string(event_kind)||',"account_id":'||app.go_json_string(effect->>'account_id')||
                ',"family_id":'||app.go_json_string(effect->>'family_id')||',"role":'||app.go_json_string(effect->>'role')||
                ',"status":'||app.go_json_string(effect->>'status')||',"operator_policy_id":'||app.go_json_string(effect->>'operator_policy_id')||
                ',"operator_evidence_digest":'||app.go_json_string(effect->>'operator_evidence_digest')||',"operator_reason_code":'||
                app.go_json_string(effect->>'operator_reason_code')||',"expected_revision":'||(effect->>'expected_revision')||'}';
        ELSE
            IF jsonb_array_length(p_event->'effects')<>1 OR (effect-ARRAY['kind','family_id','status','entitlement_key','source','expires_at','operator_policy_id','operator_evidence_digest','operator_reason_code','expected_revision']::text[])<>'{}'::jsonb
               OR NOT(effect ?& ARRAY['kind','family_id','status','entitlement_key','source','operator_policy_id','operator_evidence_digest','operator_reason_code','expected_revision'])
               OR effect->>'status'<>(CASE WHEN event_kind='operator_entitlement_granted' THEN 'active' ELSE 'suspended' END)
               OR effect->>'entitlement_key' !~ '^[a-z][a-z0-9_]{0,63}$' OR effect->>'source'<>'hosted_beta'
               OR effect->>'operator_policy_id' !~ '^[A-Za-z][A-Za-z0-9._-]{0,127}$' OR effect->>'operator_evidence_digest' !~ '^[0-9a-f]{64}$'
               OR effect->>'operator_reason_code' !~ '^[a-z][a-z0-9_]{0,63}$' OR jsonb_typeof(effect->'expected_revision')<>'number'
               OR (effect->>'expected_revision')::bigint<=0 OR (effect->>'expected_revision')::bigint<>p_revision-1 THEN
                RAISE EXCEPTION 'invalid operator entitlement effect' USING ERRCODE='22023'; END IF;
            canonical_effect:='{"kind":'||app.go_json_string(event_kind)||',"family_id":'||app.go_json_string(effect->>'family_id')||
                ',"status":'||app.go_json_string(effect->>'status')||',"entitlement_key":'||app.go_json_string(effect->>'entitlement_key')||
                ',"source":'||app.go_json_string(effect->>'source');
            IF effect ? 'expires_at' THEN canonical_effect:=canonical_effect||',"expires_at":'||app.go_json_string(effect->>'expires_at'); END IF;
            canonical_effect:=canonical_effect||',"operator_policy_id":'||app.go_json_string(effect->>'operator_policy_id')||
                ',"operator_evidence_digest":'||app.go_json_string(effect->>'operator_evidence_digest')||',"operator_reason_code":'||
                app.go_json_string(effect->>'operator_reason_code')||',"expected_revision":'||(effect->>'expected_revision')||'}';
        END CASE;
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
    IF event_kind='family_deletion_scheduled' AND (anchor IS NULL OR (SELECT count(*) FROM jsonb_array_elements(p_event->'effects') e WHERE e->>'status'='deletion_pending')<>1) THEN
        RAISE EXCEPTION 'family deletion requires one anchor' USING ERRCODE='22023'; END IF;
    IF event_kind='owner_recovered' AND (old_owner IS NULL OR new_owner IS NULL OR old_owner->>'family_id'<>new_owner->>'family_id'
       OR old_owner->>'operator_policy_id'<>new_owner->>'operator_policy_id' OR old_owner->>'operator_evidence_digest'<>new_owner->>'operator_evidence_digest'
       OR old_owner->>'operator_reason_code'<>new_owner->>'operator_reason_code' OR old_owner->>'expected_revision'<>new_owner->>'expected_revision') THEN
        RAISE EXCEPTION 'owner recovery pair mismatch' USING ERRCODE='22023'; END IF;

    SELECT generation,revision,event_hash INTO current_generation,current_revision,current_hash FROM public.security_projection_head WHERE singleton FOR UPDATE;
    IF current_generation=p_generation AND current_revision=p_revision AND current_hash=p_event_hash THEN
        IF NOT EXISTS(SELECT FROM public.security_audit_events WHERE generation=p_generation AND revision=p_revision
            AND event_hash=p_event_hash AND event_bytes=p_event_bytes AND event_data=p_event) THEN
            RAISE EXCEPTION 'projection head lacks exact audit receipt' USING ERRCODE='23514'; END IF;
        RETURN;
    END IF;
    IF current_generation<>p_generation OR current_revision+1<>p_revision OR current_hash<>p_previous_hash THEN
        RAISE EXCEPTION 'security projection head mismatch' USING ERRCODE='40001'; END IF;

    IF event_kind='family_deletion_scheduled' THEN
        family_value:=(anchor->>'family_id')::uuid; account_value:=(anchor->>'account_id')::uuid;
        PERFORM set_config('app.family_id',family_value::text,true); PERFORM set_config('app.account_id',account_value::text,true);
        IF (SELECT count(*) FROM public.memberships WHERE family_id=family_value AND status='active')<>jsonb_array_length(p_event->'effects')
           OR EXISTS(SELECT FROM public.memberships m WHERE m.family_id=family_value AND m.status='active' AND NOT EXISTS(
                SELECT FROM jsonb_array_elements(p_event->'effects') e WHERE (e->>'account_id')::uuid=m.account_id AND e->>'role'=m.role)) THEN
            RAISE EXCEPTION 'family deletion membership mismatch' USING ERRCODE='23514'; END IF;
        UPDATE public.families SET status='deletion_pending',purge_after=(anchor->>'purge_after')::timestamptz,updated_at=event_at
          WHERE id=family_value AND status='active'; GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'family not active' USING ERRCODE='23514'; END IF;
        UPDATE public.memberships SET status='removed',removed_at=event_at,updated_at=event_at WHERE family_id=family_value AND status='active';
        FOR effect IN SELECT value FROM jsonb_array_elements(p_event->'effects') LOOP
            IF (effect->>'account_id')::uuid<>account_value THEN
                PERFORM set_config('app.account_id',effect->>'account_id',true);
                DELETE FROM public.family_slots WHERE account_id=(effect->>'account_id')::uuid AND family_id=family_value;
            END IF;
        END LOOP;
        PERFORM set_config('app.account_id',account_value::text,true);
        INSERT INTO public.family_slots(account_id,family_id,state) VALUES(account_value,family_value,'family_recovery')
          ON CONFLICT(account_id) DO UPDATE SET family_id=excluded.family_id,state='family_recovery';
        INSERT INTO public.family_recovery_claims(family_id,account_id,purge_after,status,revision)
          VALUES(family_value,account_value,(anchor->>'purge_after')::timestamptz,'active',p_revision)
          ON CONFLICT(family_id) DO UPDATE SET account_id=excluded.account_id,purge_after=excluded.purge_after,
            status='active',revision=excluded.revision
          WHERE family_recovery_claims.status IN ('recovered','forfeited','expired');
        GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'recovery claim conflict' USING ERRCODE='23514'; END IF;
        UPDATE public.family_security_state SET lifecycle='deletion_pending',purge_after=(anchor->>'purge_after')::timestamptz,
          generation=p_generation,revision=p_revision,event_hash=p_event_hash
          WHERE family_id=family_value AND owner_account_id=account_value AND lifecycle='active';
        UPDATE public.sessions SET status=CASE WHEN status='material_missing' THEN status ELSE 'revoked' END,revoked_at=event_at,
          revoke_reason='family_deletion_scheduled',updated_at=event_at WHERE family_id=family_value AND revoked_at IS NULL;
        UPDATE public.invitations SET status='revoked',revoked_at=event_at,updated_at=event_at WHERE family_id=family_value AND status='active';
        UPDATE public.family_actor_label_history SET display_name='Former caregiver',former=true WHERE family_id=family_value;
        UPDATE public.family_actor_labels SET display_name='Former caregiver',former=true,updated_at=event_at WHERE family_id=family_value;
    ELSIF event_kind='family_recovered' THEN
        effect:=p_event->'effects'->0; family_value:=(effect->>'family_id')::uuid; account_value:=(effect->>'account_id')::uuid;
        PERFORM set_config('app.family_id',family_value::text,true); PERFORM set_config('app.account_id',account_value::text,true);
        UPDATE public.family_recovery_claims SET status='recovered',revision=p_revision WHERE family_id=family_value AND account_id=account_value
          AND status='active' AND event_at<purge_after; GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'family is not recoverable' USING ERRCODE='23514'; END IF;
        UPDATE public.families SET status='active',purge_after=NULL,updated_at=event_at WHERE id=family_value AND status='deletion_pending';
        INSERT INTO public.memberships(family_id,account_id,role,status,created_at,updated_at)
          VALUES(family_value,account_value,'owner','active',event_at,event_at)
          ON CONFLICT(family_id,account_id) DO UPDATE SET role='owner',status='active',removed_at=NULL,updated_at=event_at;
        UPDATE public.family_slots SET state='active' WHERE account_id=account_value AND family_id=family_value AND state='family_recovery';
        UPDATE public.family_security_state SET lifecycle='active',purge_after=NULL,generation=p_generation,revision=p_revision,event_hash=p_event_hash
          WHERE family_id=family_value AND owner_account_id=account_value AND lifecycle='deletion_pending';
        UPDATE public.family_actor_labels l SET display_name=a.display_name,former=false,updated_at=event_at FROM public.accounts a
          WHERE l.family_id=family_value AND l.account_id=account_value AND a.id=account_value AND a.status='active';
        INSERT INTO public.family_actor_label_history(family_id,account_id,display_name,former,recorded_at)
          SELECT family_value,account_value,a.display_name,false,event_at FROM public.accounts a WHERE a.id=account_value AND a.status='active';
    ELSIF event_kind='recovery_forfeited' THEN
        effect:=p_event->'effects'->0; family_value:=(effect->>'family_id')::uuid; account_value:=(effect->>'account_id')::uuid;
        PERFORM set_config('app.family_id',family_value::text,true); PERFORM set_config('app.account_id',account_value::text,true);
        UPDATE public.family_recovery_claims SET status='forfeited',revision=p_revision WHERE family_id=family_value AND account_id=account_value AND status='active';
        GET DIAGNOSTICS affected_rows=ROW_COUNT; IF affected_rows<>1 THEN RAISE EXCEPTION 'recovery claim unavailable' USING ERRCODE='23514'; END IF;
        DELETE FROM public.family_slots WHERE account_id=account_value AND family_id=family_value AND state='family_recovery';
    ELSIF event_kind='account_deleted' THEN
        effect:=p_event->'effects'->0; account_value:=(effect->>'account_id')::uuid; PERFORM set_config('app.account_id',account_value::text,true);
        SELECT * INTO identity_record FROM public.account_identities WHERE account_id=account_value AND provider='apple';
        IF FOUND AND identity_record.apple_credential_id IS NOT NULL THEN
            SELECT server_instance_id INTO server_value FROM public.devices WHERE account_id=account_value
              ORDER BY server_instance_id LIMIT 1;
            IF server_value IS NULL THEN RAISE EXCEPTION 'credential binding lacks server instance' USING ERRCODE='23514'; END IF;
            INSERT INTO public.apple_revocation_jobs(job_id,account_id,server_instance_id,credential_purpose,credential_object_id,
              credential_reference_id,credential_reference_key_id,status,attempts,next_attempt_at,created_at,updated_at,
              deletion_operation_id,deletion_command_id,operation_version)
            VALUES(app.lifecycle_operation_uuid('apple-delete-job',p_command_id),account_value,server_value,identity_record.apple_credential_purpose,identity_record.apple_credential_object_id,
              identity_record.apple_credential_id,identity_record.apple_credential_key_id,'pending',0,event_at,event_at,event_at,
              app.lifecycle_operation_uuid('apple-delete-operation',p_command_id),p_command_id,1) ON CONFLICT(account_id,server_instance_id,credential_purpose,credential_object_id,
              credential_reference_id,credential_reference_key_id) DO NOTHING;
        END IF;
        DELETE FROM public.account_identities WHERE account_id=account_value;
        UPDATE public.accounts SET display_name='',status='deleted',profile_required=false,deleted_at=event_at,updated_at=event_at
          WHERE id=account_value AND status='active'; GET DIAGNOSTICS affected_rows=ROW_COUNT;
        IF affected_rows<>1 THEN RAISE EXCEPTION 'account unavailable' USING ERRCODE='23514'; END IF;
        UPDATE public.account_security_state SET status='deleted',generation=p_generation,revision=p_revision,event_hash=p_event_hash
          WHERE account_id=account_value AND identity_key=effect->>'identity_key' AND status IN ('active','identity_unavailable');
        UPDATE public.devices SET binding_revoked_at=coalesce(binding_revoked_at,event_at),updated_at=event_at WHERE account_id=account_value;
        UPDATE public.sessions SET status=CASE WHEN status='material_missing' THEN status ELSE 'revoked' END,revoked_at=coalesce(revoked_at,event_at),
          revoke_reason='account_deleted',updated_at=event_at WHERE account_id=account_value;
        SELECT family_id INTO family_value FROM public.family_slots WHERE account_id=account_value;
        IF family_value IS NOT NULL THEN
            PERFORM set_config('app.family_id',family_value::text,true);
            UPDATE public.memberships SET status='removed',removed_at=coalesce(removed_at,event_at),updated_at=event_at
              WHERE family_id=family_value AND account_id=account_value;
            UPDATE public.family_actor_label_history SET display_name='Former caregiver',former=true WHERE family_id=family_value AND account_id=account_value;
            UPDATE public.family_actor_labels SET display_name='Former caregiver',former=true,updated_at=event_at WHERE family_id=family_value AND account_id=account_value;
        END IF;
        PERFORM set_config('app.account_id',account_value::text,true);
        DELETE FROM public.family_slots WHERE account_id=account_value;
        UPDATE public.family_recovery_claims SET status='forfeited',revision=p_revision WHERE account_id=account_value AND status='active';
    ELSIF event_kind='owner_recovered' THEN
        family_value:=(new_owner->>'family_id')::uuid; PERFORM set_config('app.family_id',family_value::text,true);
        UPDATE public.memberships SET role='caregiver',updated_at=event_at WHERE family_id=family_value AND account_id=(old_owner->>'account_id')::uuid AND role='owner' AND status='active';
        GET DIAGNOSTICS affected_rows=ROW_COUNT; IF affected_rows<>1 THEN RAISE EXCEPTION 'old owner unavailable' USING ERRCODE='23514'; END IF;
        UPDATE public.memberships SET role='owner',updated_at=event_at WHERE family_id=family_value AND account_id=(new_owner->>'account_id')::uuid AND role='caregiver' AND status='active';
        GET DIAGNOSTICS affected_rows=ROW_COUNT; IF affected_rows<>1 THEN RAISE EXCEPTION 'new owner unavailable' USING ERRCODE='23514'; END IF;
        UPDATE public.family_security_state SET owner_account_id=(new_owner->>'account_id')::uuid,generation=p_generation,revision=p_revision,event_hash=p_event_hash
          WHERE family_id=family_value AND owner_account_id=(old_owner->>'account_id')::uuid AND lifecycle='active';
        INSERT INTO public.operator_audit_events(generation,revision,command_id,operator_id,family_id,account_id,action,policy_id,evidence_digest,reason_code,expected_revision,occurred_at)
          VALUES(p_generation,p_revision,p_command_id,p_actor_account_id,family_value,(new_owner->>'account_id')::uuid,event_kind,
            new_owner->>'operator_policy_id',decode(new_owner->>'operator_evidence_digest','hex'),new_owner->>'operator_reason_code',(new_owner->>'expected_revision')::bigint,event_at);
    ELSE
        effect:=p_event->'effects'->0; family_value:=(effect->>'family_id')::uuid; PERFORM set_config('app.family_id',family_value::text,true);
        IF event_kind='operator_entitlement_granted' THEN
            INSERT INTO public.family_entitlements(family_id,entitlement_key,source,status,expires_at,created_at,updated_at)
              VALUES(family_value,effect->>'entitlement_key',effect->>'source','active',CASE WHEN effect ? 'expires_at' THEN (effect->>'expires_at')::timestamptz END,event_at,event_at)
              ON CONFLICT(family_id,entitlement_key) DO UPDATE SET source=excluded.source,status='active',expires_at=excluded.expires_at,updated_at=event_at;
        ELSE
            UPDATE public.family_entitlements SET status='suspended',updated_at=event_at WHERE family_id=family_value
              AND entitlement_key=effect->>'entitlement_key' AND source=effect->>'source' AND status='active'
              AND expires_at IS NOT DISTINCT FROM CASE WHEN effect ? 'expires_at' THEN (effect->>'expires_at')::timestamptz END;
            GET DIAGNOSTICS affected_rows=ROW_COUNT; IF affected_rows<>1 THEN RAISE EXCEPTION 'entitlement unavailable' USING ERRCODE='23514'; END IF;
        END IF;
        INSERT INTO public.operator_audit_events(generation,revision,command_id,operator_id,family_id,action,policy_id,evidence_digest,reason_code,expected_revision,occurred_at)
          VALUES(p_generation,p_revision,p_command_id,p_actor_account_id,family_value,event_kind,effect->>'operator_policy_id',
            decode(effect->>'operator_evidence_digest','hex'),effect->>'operator_reason_code',(effect->>'expected_revision')::bigint,event_at);
    END IF;

    INSERT INTO public.security_audit_events(generation,revision,event_hash,command_id,actor_account_id,kind,event_data,event_bytes)
      VALUES(p_generation,p_revision,p_event_hash,p_command_id,p_actor_account_id,event_kind,p_event,p_event_bytes);
    INSERT INTO public.security_command_receipts(actor_account_id,command_id,command_hash,generation,revision,result)
      VALUES(p_actor_account_id,p_command_id,p_command_hash,p_generation,p_revision,p_result);
    UPDATE public.security_projection_head SET generation=p_generation,revision=p_revision,event_hash=p_event_hash WHERE singleton;
    PERFORM set_config('app.family_id',previous_family,true); PERFORM set_config('app.account_id',previous_account,true);
END;
$$;

REVOKE ALL ON FUNCTION app.project_security_event(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)
    FROM PUBLIC,baby_tracker_runtime,baby_tracker_apple_revoker;
GRANT EXECUTE ON FUNCTION app.project_security_event(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)
    TO baby_tracker_projector;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
SELECT 7,principal,object_kind,object_identity,privileges FROM public.schema_privilege_manifest WHERE schema_version=6;
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges) VALUES
    (7,'baby_tracker_apple_revoker','schema','public','USAGE'),
    (7,'baby_tracker_apple_revoker','schema','app','USAGE'),
    (7,'baby_tracker_apple_revoker','function','app.claim_apple_revocation_job(text,timestamp with time zone,timestamp with time zone)','EXECUTE'),
    (7,'baby_tracker_apple_revoker','function','app.validate_apple_revocation_claim(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamp with time zone,timestamp with time zone)','EXECUTE'),
    (7,'baby_tracker_apple_revoker','function','app.apple_credential_referenced_outside_job(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamp with time zone,timestamp with time zone)','EXECUTE'),
    (7,'baby_tracker_apple_revoker','function','app.mark_apple_provider_revoked(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamp with time zone,timestamp with time zone)','EXECUTE'),
    (7,'baby_tracker_apple_revoker','function','app.begin_apple_credential_cleanup(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamp with time zone,timestamp with time zone)','EXECUTE'),
    (7,'baby_tracker_apple_revoker','function','app.complete_apple_revocation_job(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamp with time zone,timestamp with time zone)','EXECUTE'),
    (7,'baby_tracker_apple_revoker','function','app.retry_apple_revocation_job(uuid,text,text,uuid,uuid,text,uuid,uuid,text,uuid,text,text,integer,timestamp with time zone,text,timestamp with time zone,timestamp with time zone)','EXECUTE'),
    (7,'baby_tracker_apple_revoker','function','app.claim_credential_cleanup(uuid,uuid,text,uuid,text,text,text,timestamp with time zone,timestamp with time zone)','EXECUTE'),
    (7,'baby_tracker_apple_revoker','function','app.validate_credential_cleanup_claim(text,text,uuid,uuid,uuid,uuid,text,text,timestamp with time zone)','EXECUTE'),
    (7,'baby_tracker_apple_revoker','function','app.credential_cleanup_referenced(text,text,uuid,uuid,uuid,uuid,text,text,timestamp with time zone)','EXECUTE'),
    (7,'baby_tracker_apple_revoker','function','app.finish_credential_cleanup(text,text,uuid,uuid,uuid,uuid,text,text,timestamp with time zone)','EXECUTE'),
    (7,'baby_tracker_apple_revoker','function','app.retry_credential_cleanup(text,text,uuid,uuid,uuid,uuid,text,text,text,timestamp with time zone,timestamp with time zone)','EXECUTE');

UPDATE public.schema_compatibility SET current_version=7,introduced_by='000007_lifecycle.sql',recorded_at=now() WHERE singleton;
