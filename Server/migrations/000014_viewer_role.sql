-- Read-only caregivers. A third membership role that reads everything a
-- caregiver reads and originates nothing.
--
-- Purely additive and therefore deployable ahead of the code that uses it:
-- widening a CHECK constraint accepts every row the old one did, so schema 13
-- and schema 14 binaries can run against this database at the same time. No
-- existing membership changes role, and a schema 13 binary simply never emits
-- the new value.
--
-- The single-owner invariant in 000002 counts rows WHERE role = 'owner', so a
-- viewer is invisible to it and the invariant is unaffected.

-- Dropped by lookup rather than by assumed name: an inline column CHECK is
-- auto-named by PostgreSQL, and a name that does not match would leave the old
-- constraint in place. The new one would still be added, the effective rule
-- would be the AND of both, and 'viewer' would keep being rejected — with the
-- migration reporting success. Finding it by the column it constrains fails
-- loudly instead.
DO $$
DECLARE constraint_name text;
BEGIN
    SELECT con.conname INTO STRICT constraint_name
      FROM pg_constraint con
      JOIN pg_class rel ON rel.oid = con.conrelid
      JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
     WHERE nsp.nspname = 'public' AND rel.relname = 'memberships'
       AND con.contype = 'c'
       AND pg_get_constraintdef(con.oid) LIKE '%role%caregiver%';
    EXECUTE format('ALTER TABLE public.memberships DROP CONSTRAINT %I', constraint_name);
END $$;

ALTER TABLE public.memberships
    ADD CONSTRAINT memberships_role_check
    CHECK (role IN ('owner', 'caregiver', 'viewer'));

-- Invitations already carry the role they confer; it was simply pinned to
-- 'caregiver'. Widening that constraint is enough, and it means the decision is
-- made when the link is created rather than when it is redeemed. Every
-- invitation minted before this migration keeps the role it already had.
DO $$
DECLARE constraint_name text;
BEGIN
    SELECT con.conname INTO STRICT constraint_name
      FROM pg_constraint con
      JOIN pg_class rel ON rel.oid = con.conrelid
      JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
     WHERE nsp.nspname = 'public' AND rel.relname = 'invitations'
       AND con.contype = 'c'
       AND pg_get_constraintdef(con.oid) LIKE '%role%caregiver%';
    EXECUTE format('ALTER TABLE public.invitations DROP CONSTRAINT %I', constraint_name);
END $$;

ALTER TABLE public.invitations
    ADD CONSTRAINT invitations_role_check
    CHECK (role IN ('caregiver', 'viewer'));

-- The authority the write path consults. Kept as a function rather than
-- repeated predicates so there is exactly one definition of "may write", and
-- so a future role is added in one place.
CREATE OR REPLACE FUNCTION app.membership_may_write(target_family uuid, target_account uuid)
RETURNS boolean
LANGUAGE sql
STABLE
AS $$
    SELECT EXISTS (
        SELECT 1 FROM public.memberships
         WHERE family_id = target_family
           AND account_id = target_account
           AND status = 'active'
           AND role IN ('owner', 'caregiver')
    );
$$;

-- PostgreSQL grants EXECUTE on a new function to PUBLIC by default, so the
-- grant alone would have handed this to every role including the restricted
-- projector and apple-revoker principals. Revoked first, exactly as every
-- other app function in this schema does.
REVOKE ALL ON FUNCTION app.membership_may_write(uuid,uuid)
    FROM PUBLIC, baby_tracker_projector, baby_tracker_apple_revoker;
GRANT EXECUTE ON FUNCTION app.membership_may_write(uuid,uuid) TO baby_tracker_runtime;

INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
SELECT 14,principal,object_kind,object_identity,privileges
  FROM public.schema_privilege_manifest WHERE schema_version=13;
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges) VALUES
    (14,'baby_tracker_runtime','function','app.membership_may_write(uuid,uuid)','EXECUTE');

UPDATE public.schema_compatibility
   SET current_version=14,introduced_by='000014_viewer_role.sql',recorded_at=now()
 WHERE singleton;
