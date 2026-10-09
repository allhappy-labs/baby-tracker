-- Operator/bootstrap step; run with role-administration privileges before
-- migrations. Supply distinct LOGIN principals and credentials outside this file:
-- grant baby_tracker_runtime to the server principal, never to the migrator.
-- Live Activity workers use a distinct LOGIN granted baby_tracker_live_activity;
-- never share runtime, projector, or maintenance credentials with that worker.
-- Maintenance is reserved for narrowly granted worker operations.
-- Compatible reruns are allowed. Never retain unsafe existing attributes or
-- outgoing memberships; fail before migrations can grant application access.
DO $$
DECLARE
    group_role record;
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'baby_tracker_runtime') THEN
        CREATE ROLE baby_tracker_runtime NOLOGIN NOSUPERUSER NOCREATEDB
            NOCREATEROLE NOREPLICATION NOBYPASSRLS;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'baby_tracker_maintenance') THEN
        CREATE ROLE baby_tracker_maintenance NOLOGIN NOSUPERUSER NOCREATEDB
            NOCREATEROLE NOREPLICATION NOBYPASSRLS;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'baby_tracker_projector') THEN
        CREATE ROLE baby_tracker_projector NOLOGIN NOSUPERUSER NOCREATEDB
            NOCREATEROLE NOREPLICATION NOBYPASSRLS;
    END IF;
	IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'baby_tracker_apple_revoker') THEN
		CREATE ROLE baby_tracker_apple_revoker NOLOGIN NOSUPERUSER NOCREATEDB
			NOCREATEROLE NOREPLICATION NOBYPASSRLS;
	END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'baby_tracker_live_activity') THEN
        CREATE ROLE baby_tracker_live_activity NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
    END IF;
    FOR group_role IN
        SELECT * FROM pg_catalog.pg_roles
        WHERE rolname IN ('baby_tracker_runtime', 'baby_tracker_maintenance', 'baby_tracker_projector', 'baby_tracker_apple_revoker', 'baby_tracker_live_activity')
    LOOP
        IF group_role.rolcanlogin OR NOT group_role.rolinherit OR group_role.rolsuper OR group_role.rolcreatedb
           OR group_role.rolcreaterole OR group_role.rolreplication OR group_role.rolbypassrls
           OR group_role.rolconfig IS NOT NULL
           OR (group_role.rolvaliduntil IS NOT NULL AND group_role.rolvaliduntil <> 'infinity'::timestamptz)
           OR EXISTS (SELECT FROM pg_catalog.pg_auth_members WHERE member = group_role.oid) THEN
            RAISE EXCEPTION 'unsafe existing database group role: %', group_role.rolname
                USING ERRCODE = '42501',
                    HINT = 'Require NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS and no membership in other roles.';
        END IF;
    END LOOP;
END
$$;
