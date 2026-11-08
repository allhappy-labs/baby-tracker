-- 000036_request_authority_family_key: app.request_authority, which every
-- request authenticates through, finds the family by its key instead of
-- reading the whole families table (Phase 5 Task 21, the first 10,000-family
-- measurement: 84% of database time, 33.7 ms a call under load).
--
-- The function's tables force row level security, so its owner reads them
-- through their policies, and PostgreSQL uses a join clause as an index key
-- past a policy only when the clause cannot leak a row: every node in it must
-- be one the planner knows is leakproof. A COALESCE is not one of those, so
-- `f.id = coalesce(s.family_id, fs.family_id)` stayed a join filter over every
-- family the policy admits (families_pkey end to end, 5,929 rows a call). The
-- same choice written as a CASE (with an IS NOT NULL test, both leakproof)
-- keys the families_pkey lookup. Nothing else changes: the signature, the
-- definer and its search_path, app.authority set and restored, the match
-- count, every returned column and the context the function sets. CREATE OR
-- REPLACE keeps the function's owner and grants, so the privilege manifest is
-- 35's unchanged.
CREATE OR REPLACE FUNCTION app.request_authority(p_access_digests bytea[], p_session_id uuid, p_family boolean, p_recovery_epoch bigint) RETURNS TABLE(matches integer, access_digest bytea, token_key_id text, session_id uuid, account_id uuid, device_id uuid, scope text, session_family_id uuid, session_role text, session_status text, recovery_epoch bigint, issued_seq bigint, access_expires_at timestamp with time zone, deadline_at timestamp with time zone, lineage_started_at timestamp with time zone, account_status text, account_chain_failed boolean, slot_family_id uuid, slot_state text, member_role text, member_status text, family_id uuid, family_status text, family_audit_seq bigint, family_chain_failed boolean, family_entitlement_required boolean, entitlement_status text, entitlement_expires_at timestamp with time zone)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog'
    AS $$
#variable_conflict use_column
DECLARE
  previous_authority text := coalesce(current_setting('app.authority',true),'');
  v_digest bytea;
BEGIN
  IF p_family IS NULL OR p_recovery_epoch IS NULL OR (p_access_digests IS NULL) = (p_session_id IS NULL)
     OR (p_access_digests IS NOT NULL AND cardinality(p_access_digests) NOT BETWEEN 1 AND 8) THEN
    RAISE EXCEPTION 'invalid request authority lookup' USING ERRCODE='22023';
  END IF;
  PERFORM set_config('app.authority','1',true);
  IF p_session_id IS NULL THEN
    SELECT count(*) OVER (), s.access_digest INTO matches, v_digest
      FROM public.sessions s WHERE s.access_digest = ANY(p_access_digests) LIMIT 1;
  ELSE
    SELECT 1, s.access_digest INTO matches, v_digest FROM public.sessions s WHERE s.id = p_session_id;
  END IF;
  matches := least(coalesce(matches,0),2);
  IF matches > 0 THEN
    -- The family key is coalesce(s.family_id, fs.family_id) written as a
    -- CASE: a COALESCE here is not an index key under the families policy
    -- (see the header), and the lookup reads every family.
    SELECT s.access_digest, s.token_key_id, s.id, s.account_id, s.device_id, s.scope::text, s.family_id, s.role::text, s.status::text,
           s.recovery_epoch, s.issued_seq, s.access_expires_at, s.deadline_at, s.lineage_started_at, a.status::text, a.chain_failed,
           fs.family_id, fs.state::text, m.role::text, m.status::text, f.id, f.status::text, f.audit_seq, f.chain_failed,
           f.entitlement_required, e.status::text, e.expires_at
      INTO access_digest, token_key_id, session_id, account_id, device_id, scope, session_family_id, session_role, session_status,
           recovery_epoch, issued_seq, access_expires_at, deadline_at, lineage_started_at, account_status, account_chain_failed,
           slot_family_id, slot_state, member_role, member_status, family_id, family_status, family_audit_seq, family_chain_failed,
           family_entitlement_required, entitlement_status, entitlement_expires_at
      FROM public.sessions s
      JOIN public.accounts a ON a.id = s.account_id
      LEFT JOIN public.family_slots fs ON fs.account_id = s.account_id
      LEFT JOIN public.memberships m ON m.family_id = fs.family_id AND m.account_id = s.account_id
      LEFT JOIN public.families f ON p_family
        AND f.id = CASE WHEN s.family_id IS NOT NULL THEN s.family_id ELSE fs.family_id END
      LEFT JOIN public.family_entitlements e ON e.family_id = f.id AND e.entitlement_key = 'family_sync'
     WHERE s.access_digest = v_digest;
  END IF;
  PERFORM set_config('app.authority',previous_authority,true);
  IF matches = 1 AND session_status = 'active' AND account_status = 'active' AND recovery_epoch = p_recovery_epoch
     AND NOT account_chain_failed THEN
    PERFORM set_config('app.account_id',account_id::text,true);
    PERFORM set_config('app.family_id',
      CASE WHEN p_family AND family_status = 'active' AND NOT family_chain_failed AND scope <> 'family_recovery' THEN family_id::text ELSE '' END,
      true);
  ELSE
    PERFORM set_config('app.account_id','',true);
    PERFORM set_config('app.family_id','',true);
  END IF;
  RETURN NEXT;
END $$;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT 36,principal,object_kind,object_identity,privileges
 FROM public.schema_privilege_manifest WHERE schema_version=35;
UPDATE public.schema_compatibility SET current_version=36,
 introduced_by='000036_request_authority_family_key.sql',recorded_at=now() WHERE singleton;
