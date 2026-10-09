-- This migration runs as the table-owning migrator, not the runtime principal.
-- The group roles are provisioned separately by deploy/postgres/init-roles.sql.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
DO $$
BEGIN
    EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', current_database());
END
$$;

CREATE SCHEMA app;
REVOKE ALL ON SCHEMA app FROM PUBLIC;
GRANT USAGE ON SCHEMA public, app TO baby_tracker_runtime;

CREATE FUNCTION app.current_account_id() RETURNS uuid
LANGUAGE sql STABLE
SET search_path = pg_catalog
AS $$ SELECT nullif(current_setting('app.account_id', true), '')::uuid $$;

CREATE FUNCTION app.current_family_id() RETURNS uuid
LANGUAGE sql STABLE
SET search_path = pg_catalog
AS $$ SELECT nullif(current_setting('app.family_id', true), '')::uuid $$;

REVOKE ALL ON FUNCTION app.current_account_id(), app.current_family_id() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.current_account_id(), app.current_family_id() TO baby_tracker_runtime;

CREATE TABLE accounts (
    id uuid PRIMARY KEY CHECK (id <> '00000000-0000-0000-0000-000000000000'),
    display_name text NOT NULL,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'deleted')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);

CREATE TABLE families (
    id uuid PRIMARY KEY CHECK (id <> '00000000-0000-0000-0000-000000000000'),
    display_name text NOT NULL,
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'deletion_pending')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    purge_after timestamptz
);

CREATE TABLE memberships (
    family_id uuid NOT NULL REFERENCES families(id) ON DELETE CASCADE,
    account_id uuid NOT NULL REFERENCES accounts(id),
    role text NOT NULL CHECK (role IN ('owner', 'caregiver')),
    status text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'removed')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    removed_at timestamptz,
    PRIMARY KEY (family_id, account_id)
);
CREATE INDEX memberships_account_id_idx ON memberships(account_id);

ALTER TABLE accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE accounts FORCE ROW LEVEL SECURITY;
ALTER TABLE families ENABLE ROW LEVEL SECURITY;
ALTER TABLE families FORCE ROW LEVEL SECURITY;
ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships FORCE ROW LEVEL SECURITY;

CREATE POLICY account_context ON accounts
    USING (id = app.current_account_id())
    WITH CHECK (id = app.current_account_id());
CREATE POLICY family_context ON families
    USING (id = app.current_family_id())
    WITH CHECK (id = app.current_family_id());
CREATE POLICY membership_context ON memberships
    USING (family_id = app.current_family_id())
    WITH CHECK (family_id = app.current_family_id());

GRANT SELECT, INSERT, UPDATE, DELETE ON accounts, families, memberships TO baby_tracker_runtime;
-- Readiness inspects migration history and server identity; runtime never writes
-- either operational table or performs migrations.
GRANT SELECT ON schema_migrations, server_state TO baby_tracker_runtime;

-- All membership mutations write the parent row to serialize owner decisions.
-- A row write (not just an advisory/row lock) also prevents stale-snapshot write
-- skew under REPEATABLE READ. Sorted locking handles a move between families.
-- These trigger-only definers set their own family context, so deferred checks
-- still see the affected family if the caller changes context before commit.
-- Each function restores the caller's setting on normal exit; a raised error
-- rolls its changes back with the failing statement/transaction.
CREATE FUNCTION app.lock_membership_families() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    previous_family text := coalesce(current_setting('app.family_id', true), '');
    affected uuid;
    old_family uuid;
    new_family uuid;
BEGIN
    IF TG_OP <> 'INSERT' THEN old_family := OLD.family_id; END IF;
    IF TG_OP <> 'DELETE' THEN new_family := NEW.family_id; END IF;
    FOR affected IN SELECT DISTINCT id FROM unnest(ARRAY[old_family, new_family]) AS ids(id)
                    WHERE id IS NOT NULL ORDER BY id
    LOOP
        PERFORM set_config('app.family_id', affected::text, true);
        UPDATE public.families SET updated_at = updated_at WHERE id = affected;
    END LOOP;
    PERFORM set_config('app.family_id', previous_family, true);
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION app.check_family_owner() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    previous_family text := coalesce(current_setting('app.family_id', true), '');
    affected uuid;
    old_family uuid;
    new_family uuid;
BEGIN
    IF TG_TABLE_NAME = 'families' THEN
        IF TG_OP <> 'INSERT' THEN old_family := OLD.id; END IF;
        IF TG_OP <> 'DELETE' THEN new_family := NEW.id; END IF;
    ELSE
        IF TG_OP <> 'INSERT' THEN old_family := OLD.family_id; END IF;
        IF TG_OP <> 'DELETE' THEN new_family := NEW.family_id; END IF;
    END IF;
    FOR affected IN SELECT DISTINCT id FROM unnest(ARRAY[old_family, new_family]) AS ids(id)
                    WHERE id IS NOT NULL ORDER BY id
    LOOP
        PERFORM set_config('app.family_id', affected::text, true);
        IF EXISTS (SELECT FROM public.families WHERE id = affected AND status = 'active')
           AND (SELECT count(*) FROM public.memberships
                WHERE family_id = affected AND role = 'owner' AND status = 'active') <> 1 THEN
            RAISE EXCEPTION 'active family must have exactly one active owner'
                USING ERRCODE = '23514', CONSTRAINT = 'family_exactly_one_active_owner';
        END IF;
    END LOOP;
    PERFORM set_config('app.family_id', previous_family, true);
    RETURN NULL;
END
$$;

REVOKE ALL ON FUNCTION app.lock_membership_families(), app.check_family_owner() FROM PUBLIC;

CREATE TRIGGER membership_family_lock
    BEFORE INSERT OR UPDATE OR DELETE ON memberships
    FOR EACH ROW EXECUTE FUNCTION app.lock_membership_families();
CREATE CONSTRAINT TRIGGER family_owner_from_family
    AFTER INSERT OR UPDATE ON families DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION app.check_family_owner();
CREATE CONSTRAINT TRIGGER family_owner_from_membership
    AFTER INSERT OR UPDATE OR DELETE ON memberships DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION app.check_family_owner();
