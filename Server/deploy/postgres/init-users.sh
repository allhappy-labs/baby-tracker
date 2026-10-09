#!/bin/sh
# Official PostgreSQL init-only bootstrap. Never pass these admin credentials to
# app/migrate. Passwords stay in server-side variables, not argv or printed SQL.
set -eu
psql -X -v ON_ERROR_STOP=1 --username postgres --dbname babytracker <<'SQL'
BEGIN;
\i /docker-entrypoint-initdb.d/10-init-roles.sql
DO $$
DECLARE
    principal record;
    principal_name text;
    secret text;
    manifest_absent boolean := to_regclass('public.schema_privilege_manifest') IS NULL;
BEGIN
    FOREACH principal_name IN ARRAY ARRAY['baby_tracker_runtime', 'baby_tracker_maintenance', 'baby_tracker_projector', 'baby_tracker_apple_revoker', 'baby_tracker_live_activity', 'baby_tracker_migrator', 'baby_tracker_app', 'baby_tracker_export_app', 'baby_tracker_projector_app', 'baby_tracker_apple_revoker_app', 'baby_tracker_live_activity_app'] LOOP
        SELECT * INTO principal FROM pg_roles WHERE rolname = principal_name;
        IF FOUND THEN
            -- Group attributes/memberships were checked by init-roles.sql.
            IF principal_name NOT IN ('baby_tracker_runtime', 'baby_tracker_maintenance', 'baby_tracker_projector', 'baby_tracker_apple_revoker', 'baby_tracker_live_activity') AND (
               NOT principal.rolcanlogin OR NOT principal.rolinherit OR principal.rolsuper
               OR principal.rolcreatedb OR principal.rolcreaterole OR principal.rolreplication
               OR principal.rolbypassrls OR principal.rolconfig IS NOT NULL
               OR (principal.rolvaliduntil IS NOT NULL AND principal.rolvaliduntil <> 'infinity'::timestamptz)
               OR EXISTS (
                   SELECT FROM pg_auth_members m JOIN pg_roles parent ON parent.oid = m.roleid
                   WHERE m.member = principal.oid AND
                     (NOT ((principal_name = 'baby_tracker_app' AND parent.rolname = 'baby_tracker_runtime')
                        OR (principal_name = 'baby_tracker_export_app' AND parent.rolname = 'baby_tracker_maintenance')
                        OR (principal_name = 'baby_tracker_projector_app' AND parent.rolname = 'baby_tracker_projector')
                        OR (principal_name = 'baby_tracker_apple_revoker_app' AND parent.rolname = 'baby_tracker_apple_revoker')
                        OR (principal_name = 'baby_tracker_live_activity_app' AND parent.rolname = 'baby_tracker_live_activity'))
                      OR m.admin_option OR NOT m.inherit_option OR NOT m.set_option)
               )) THEN
                RAISE EXCEPTION 'incompatible existing application principal' USING ERRCODE = '42501';
            END IF;
            -- Membership checks cannot detect direct ACLs or object ownership.
            -- Inspect grants to both the LOGIN and its runtime group; effective privilege helpers
            -- would also see legitimate PUBLIC/runtime-group grants. Reject
            -- before changing ownership/ACLs, and let the transaction roll back.
            IF principal_name IN ('baby_tracker_app', 'baby_tracker_export_app', 'baby_tracker_projector_app', 'baby_tracker_apple_revoker_app', 'baby_tracker_live_activity_app', 'baby_tracker_runtime', 'baby_tracker_maintenance', 'baby_tracker_projector', 'baby_tracker_apple_revoker', 'baby_tracker_live_activity') AND (
                EXISTS (
                    SELECT FROM pg_catalog.pg_database d
                    WHERE d.datname = current_database() AND (
                        d.datdba = principal.oid OR EXISTS (
                            SELECT FROM pg_catalog.aclexplode(d.datacl) acl
                            WHERE acl.grantee = principal.oid
                              AND acl.privilege_type IN ('CREATE', 'TEMPORARY')
                        )
                    )
                ) OR EXISTS (
                    SELECT FROM pg_catalog.pg_namespace n
                    WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
                      AND (n.nspowner = principal.oid OR EXISTS (
                          SELECT FROM pg_catalog.aclexplode(n.nspacl) acl
                          WHERE acl.grantee = principal.oid AND (
                              principal_name IN ('baby_tracker_app', 'baby_tracker_export_app', 'baby_tracker_projector_app', 'baby_tracker_apple_revoker_app', 'baby_tracker_live_activity_app')
                              OR (manifest_absent AND NOT (acl.privilege_type='USAGE' AND n.nspname IN ('public','app')))
                          )
                      ))
                ) OR EXISTS (
                    SELECT FROM pg_catalog.pg_class c
                    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
                    WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
                      AND c.relpersistence <> 't' AND (
                          c.relowner = principal.oid OR EXISTS (
                              SELECT FROM pg_catalog.aclexplode(c.relacl) acl
                              WHERE acl.grantee = principal.oid
                                AND (principal_name IN ('baby_tracker_app', 'baby_tracker_export_app', 'baby_tracker_projector_app', 'baby_tracker_apple_revoker_app', 'baby_tracker_live_activity_app') OR
                                  (manifest_absent AND NOT (
                                    (principal_name='baby_tracker_runtime' AND n.nspname='public' AND c.relkind IN ('r','p')
                                      AND c.relname IN ('accounts','families','memberships') AND acl.privilege_type IN ('SELECT','INSERT','UPDATE','DELETE'))
                                    OR (principal_name='baby_tracker_runtime' AND n.nspname='public'
                                      AND c.relname IN ('schema_migrations','server_state') AND acl.privilege_type='SELECT')
                                    OR (principal_name='baby_tracker_projector' AND n.nspname='public'
                                      AND c.relname='security_projection_head' AND acl.privilege_type='SELECT')
                                  )))
                          ) OR EXISTS (
                              SELECT FROM pg_catalog.pg_attribute a,
                                  LATERAL pg_catalog.aclexplode(a.attacl) acl
                              WHERE a.attrelid = c.oid AND acl.grantee = principal.oid
                                AND (principal_name IN ('baby_tracker_app', 'baby_tracker_export_app', 'baby_tracker_projector_app', 'baby_tracker_apple_revoker_app', 'baby_tracker_live_activity_app') OR manifest_absent)
                          )
                      )
                ) OR EXISTS (
                    -- Ownership dependencies include every object catalog,
                    -- including objects beyond tables/functions/types (for
                    -- example collations, operators and large objects).
                    SELECT FROM pg_catalog.pg_shdepend d
                    WHERE d.refclassid = 'pg_catalog.pg_authid'::regclass
                      AND d.refobjid = principal.oid AND d.deptype = 'o'
                      AND d.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
                ) OR EXISTS (
                    SELECT FROM pg_catalog.pg_proc p
                    JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
                    WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' AND (
                        p.proowner = principal.oid OR EXISTS (
                            SELECT FROM pg_catalog.aclexplode(p.proacl) acl
                            WHERE acl.grantee = principal.oid
                              AND (principal_name IN ('baby_tracker_app', 'baby_tracker_export_app', 'baby_tracker_projector_app', 'baby_tracker_apple_revoker_app', 'baby_tracker_live_activity_app') OR
                                (manifest_absent AND NOT (
                                  (principal_name='baby_tracker_runtime' AND p.oid IN (
                                    to_regprocedure('app.current_account_id()'),to_regprocedure('app.current_family_id()')))
                                  OR (principal_name='baby_tracker_projector' AND p.oid =
                                    to_regprocedure('app.project_security_event(bigint,bigint,bytea,bytea,uuid,uuid,bytea,jsonb,jsonb,bytea)'))
                                )))
                        ))
                ) OR EXISTS (
                    SELECT FROM pg_catalog.pg_type t
                    JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
                    WHERE t.typowner = principal.oid
                      AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
                ) OR EXISTS (
                    SELECT FROM pg_catalog.pg_default_acl d,
                        LATERAL pg_catalog.aclexplode(d.defaclacl) acl
                    WHERE d.defaclrole = principal.oid OR acl.grantee IN (0, principal.oid)
                )
            ) THEN
                RAISE EXCEPTION 'incompatible existing runtime privileges or ownership' USING ERRCODE = '42501';
            END IF;
        ELSE
            EXECUTE format('CREATE ROLE %I LOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS', principal_name);
        END IF;
        IF principal_name IN ('baby_tracker_runtime', 'baby_tracker_maintenance', 'baby_tracker_projector', 'baby_tracker_apple_revoker', 'baby_tracker_live_activity') THEN
            CONTINUE;
        END IF;
        -- Schema-7 compatibility may be deployed before the hosted worker
        -- secret is configured. Keep the distinct LOGIN passwordless (and
        -- therefore unable to authenticate); a later idempotent bootstrap
        -- installs its password as soon as the secret is mounted.
        IF principal_name = 'baby_tracker_apple_revoker_app'
           AND pg_stat_file('/run/secrets/apple_revocation_password', true) IS NULL THEN
            CONTINUE;
        END IF;
        IF principal_name = 'baby_tracker_export_app'
           AND pg_stat_file('/run/secrets/export_password', true) IS NULL THEN
            CONTINUE;
        END IF;
        IF principal_name = 'baby_tracker_live_activity_app'
           AND pg_stat_file('/run/secrets/live_activity_password', true) IS NULL THEN
            CONTINUE;
        END IF;
        BEGIN
            secret := pg_read_file(CASE principal_name
                WHEN 'baby_tracker_migrator' THEN '/run/secrets/migrator_password'
                WHEN 'baby_tracker_projector_app' THEN '/run/secrets/projector_password'
                WHEN 'baby_tracker_apple_revoker_app' THEN '/run/secrets/apple_revocation_password'
                WHEN 'baby_tracker_export_app' THEN '/run/secrets/export_password'
                WHEN 'baby_tracker_live_activity_app' THEN '/run/secrets/live_activity_password'
                ELSE '/run/secrets/runtime_password' END, 0, 16385);
        EXCEPTION WHEN OTHERS THEN
            RAISE EXCEPTION 'application credential file could not be read';
        END;
        IF octet_length(secret) > 16384 THEN
            RAISE EXCEPTION 'invalid application credential file';
        END IF;
        secret := regexp_replace(secret, E'\r?\n$', '');
        IF secret = '' OR secret ~ E'[\r\n]' THEN
            RAISE EXCEPTION 'invalid application credential file';
        END IF;
        EXECUTE format('ALTER ROLE %I PASSWORD %L', principal_name, secret);
    END LOOP;
    IF (SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = 'babytracker')
       NOT IN ('postgres', 'baby_tracker_migrator') THEN
        RAISE EXCEPTION 'incompatible existing application database owner' USING ERRCODE = '42501';
    END IF;
END
$$;
GRANT baby_tracker_runtime TO baby_tracker_app;
GRANT baby_tracker_maintenance TO baby_tracker_export_app;
GRANT baby_tracker_projector TO baby_tracker_projector_app;
GRANT baby_tracker_apple_revoker TO baby_tracker_apple_revoker_app;
GRANT baby_tracker_live_activity TO baby_tracker_live_activity_app;
DO $$
DECLARE
    expected record;
BEGIN
    FOR expected IN SELECT * FROM (VALUES
        ('baby_tracker_runtime'::text, 'baby_tracker_app'::text),
        ('baby_tracker_maintenance'::text, 'baby_tracker_export_app'::text),
        ('baby_tracker_projector'::text, 'baby_tracker_projector_app'::text),
        ('baby_tracker_apple_revoker'::text, 'baby_tracker_apple_revoker_app'::text),
        ('baby_tracker_live_activity'::text, 'baby_tracker_live_activity_app'::text)
    ) AS pairs(group_name, login_name)
    LOOP
        IF NOT (
            SELECT count(*)=1 AND bool_and(member.rolname=expected.login_name AND NOT membership.admin_option
                AND membership.inherit_option AND membership.set_option)
            FROM pg_auth_members membership
            JOIN pg_roles parent ON parent.oid=membership.roleid
            JOIN pg_roles member ON member.oid=membership.member
            WHERE parent.rolname=expected.group_name
        ) THEN
            RAISE EXCEPTION 'application group has unexpected incoming membership' USING ERRCODE='42501';
        END IF;
    END LOOP;
END
$$;
ALTER DATABASE babytracker OWNER TO baby_tracker_migrator;
REVOKE ALL ON DATABASE babytracker FROM PUBLIC;
GRANT CONNECT ON DATABASE babytracker TO baby_tracker_runtime, baby_tracker_maintenance, baby_tracker_projector, baby_tracker_apple_revoker, baby_tracker_live_activity;
ALTER SCHEMA public OWNER TO baby_tracker_migrator;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;

SELECT CASE WHEN to_regclass('public.schema_privilege_manifest') IS NULL THEN 'false' ELSE 'true' END AS has_privilege_manifest \gset
\if :has_privilege_manifest
DO $$
DECLARE
    principal_name text;
    compatible_version bigint;
BEGIN
    SELECT current_version INTO STRICT compatible_version FROM schema_compatibility WHERE singleton;
    CREATE TEMP TABLE observed_privileges (
        principal text NOT NULL,
        object_kind text NOT NULL,
        object_identity text NOT NULL,
        privileges text NOT NULL,
        PRIMARY KEY (principal, object_kind, object_identity)
    ) ON COMMIT DROP;
    FOREACH principal_name IN ARRAY CASE WHEN compatible_version >= 28
        THEN ARRAY['baby_tracker_runtime','baby_tracker_maintenance','baby_tracker_projector','baby_tracker_apple_revoker','baby_tracker_live_activity']
        WHEN compatible_version >= 19
        THEN ARRAY['baby_tracker_runtime','baby_tracker_maintenance','baby_tracker_projector','baby_tracker_apple_revoker']
        WHEN compatible_version >= 7
        THEN ARRAY['baby_tracker_runtime','baby_tracker_projector','baby_tracker_apple_revoker']
        ELSE ARRAY['baby_tracker_runtime','baby_tracker_projector'] END LOOP
        INSERT INTO observed_privileges
        SELECT principal_name, 'table', identity, privileges FROM (
            SELECT n.nspname || '.' || c.relname AS identity,
                concat_ws(',',
                    CASE WHEN has_table_privilege(principal_name,c.oid,'SELECT') THEN 'SELECT' END,
                    CASE WHEN has_table_privilege(principal_name,c.oid,'INSERT') THEN 'INSERT' END,
                    CASE WHEN has_table_privilege(principal_name,c.oid,'UPDATE') THEN 'UPDATE' END,
                    CASE WHEN has_table_privilege(principal_name,c.oid,'DELETE') THEN 'DELETE' END,
                    CASE WHEN has_table_privilege(principal_name,c.oid,'TRUNCATE') THEN 'TRUNCATE' END,
                    CASE WHEN has_table_privilege(principal_name,c.oid,'REFERENCES') THEN 'REFERENCES' END,
                    CASE WHEN has_table_privilege(principal_name,c.oid,'TRIGGER') THEN 'TRIGGER' END,
                    CASE WHEN has_table_privilege(principal_name,c.oid,'MAINTAIN') THEN 'MAINTAIN' END) AS privileges
            FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
            WHERE n.nspname !~ '^pg_' AND n.nspname<>'information_schema' AND c.relpersistence<>'t' AND c.relkind IN ('r','p','v','m','f')
        ) inventory WHERE privileges<>'';
        INSERT INTO observed_privileges
        SELECT principal_name, 'column', identity, privileges FROM (
            SELECT n.nspname || '.' || c.relname || '.' || a.attname AS identity,
                concat_ws(',',
                    CASE WHEN NOT has_table_privilege(principal_name,c.oid,'SELECT') AND has_column_privilege(principal_name,c.oid,a.attnum,'SELECT') THEN 'SELECT' END,
                    CASE WHEN NOT has_table_privilege(principal_name,c.oid,'INSERT') AND has_column_privilege(principal_name,c.oid,a.attnum,'INSERT') THEN 'INSERT' END,
                    CASE WHEN NOT has_table_privilege(principal_name,c.oid,'UPDATE') AND has_column_privilege(principal_name,c.oid,a.attnum,'UPDATE') THEN 'UPDATE' END,
                    CASE WHEN NOT has_table_privilege(principal_name,c.oid,'REFERENCES') AND has_column_privilege(principal_name,c.oid,a.attnum,'REFERENCES') THEN 'REFERENCES' END) AS privileges
            FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_namespace n ON n.oid=c.relnamespace
            WHERE a.attnum>0 AND NOT a.attisdropped AND n.nspname !~ '^pg_' AND n.nspname<>'information_schema'
              AND c.relpersistence<>'t' AND c.relkind IN ('r','p','v','m','f')
        ) inventory WHERE privileges<>'';
        INSERT INTO observed_privileges
        SELECT principal_name, 'sequence', identity, privileges FROM (
            SELECT n.nspname || '.' || c.relname AS identity,
                concat_ws(',',
                    CASE WHEN has_sequence_privilege(principal_name,c.oid,'SELECT') THEN 'SELECT' END,
                    CASE WHEN has_sequence_privilege(principal_name,c.oid,'UPDATE') THEN 'UPDATE' END,
                    CASE WHEN has_sequence_privilege(principal_name,c.oid,'USAGE') THEN 'USAGE' END) AS privileges
            FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
            WHERE c.relkind='S' AND n.nspname !~ '^pg_' AND n.nspname<>'information_schema'
        ) inventory WHERE privileges<>'';
        INSERT INTO observed_privileges
        SELECT principal_name, 'schema', identity, privileges FROM (
            SELECT n.nspname AS identity, concat_ws(',',
                CASE WHEN has_schema_privilege(principal_name,n.oid,'USAGE') THEN 'USAGE' END,
                CASE WHEN has_schema_privilege(principal_name,n.oid,'CREATE') THEN 'CREATE' END) AS privileges
            FROM pg_namespace n WHERE n.nspname !~ '^pg_' AND n.nspname<>'information_schema'
        ) inventory WHERE privileges<>'';
        INSERT INTO observed_privileges
        SELECT principal_name, 'function', p.oid::regprocedure::text, 'EXECUTE'
        FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
        WHERE n.nspname !~ '^pg_' AND n.nspname<>'information_schema'
          AND has_function_privilege(principal_name,p.oid,'EXECUTE');
    END LOOP;
    IF EXISTS (
        (SELECT principal,object_kind,object_identity,privileges FROM observed_privileges
         EXCEPT SELECT principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=compatible_version)
        UNION ALL
        (SELECT principal,object_kind,object_identity,privileges FROM schema_privilege_manifest WHERE schema_version=compatible_version
         EXCEPT SELECT principal,object_kind,object_identity,privileges FROM observed_privileges)
    ) THEN
        RAISE EXCEPTION 'effective privileges do not match schema privilege manifest' USING ERRCODE='42501';
    END IF;
END
$$;
\endif
COMMIT;
SQL
