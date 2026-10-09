#!/bin/sh
# Runs only inside the disposable smoke database container, as bootstrap admin.
set -eu
sql() { psql -X -qAt -v ON_ERROR_STOP=1 -U postgres -d babytracker -c "$1"; }
negative_probes=${BT_PRINCIPAL_NEGATIVE_PROBES:-1}
case "$negative_probes" in 0|1) ;; *) echo 'BT_PRINCIPAL_NEGATIVE_PROBES must be 0 or 1' >&2; exit 2;; esac
# The negative bootstrap probes intentionally raise errors from a large DO
# block. Keep these expected errors out of the PostgreSQL server log; on musl
# builds a closed log pipe can otherwise deliver SIGPIPE to the backend and
# force crash recovery during the smoke. The client still receives and drains
# each error, so the bootstrap exit status remains the assertion under test.
bootstrap() {
  PGOPTIONS='-c log_min_messages=panic -c log_min_error_statement=panic -c log_error_verbosity=terse' \
    sh /docker-entrypoint-initdb.d/20-init-users.sh
}
bootstrap
bootstrap
test "$(sql "SELECT count(*) FROM pg_roles WHERE rolname IN ('baby_tracker_migrator','baby_tracker_app','baby_tracker_export_app','baby_tracker_projector_app','baby_tracker_apple_revoker_app') AND rolcanlogin AND NOT (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)")" = 5
test "$(sql "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='babytracker'")" = baby_tracker_migrator
test "$(sql "SELECT string_agg(parent.rolname, ',') FROM pg_auth_members m JOIN pg_roles child ON child.oid=m.member JOIN pg_roles parent ON parent.oid=m.roleid WHERE child.rolname='baby_tracker_app'")" = baby_tracker_runtime
test "$(sql "SELECT string_agg(parent.rolname, ',') FROM pg_auth_members m JOIN pg_roles child ON child.oid=m.member JOIN pg_roles parent ON parent.oid=m.roleid WHERE child.rolname='baby_tracker_export_app'")" = baby_tracker_maintenance
test "$(sql "SELECT string_agg(parent.rolname, ',') FROM pg_auth_members m JOIN pg_roles child ON child.oid=m.member JOIN pg_roles parent ON parent.oid=m.roleid WHERE child.rolname='baby_tracker_projector_app'")" = baby_tracker_projector
test "$(sql "SELECT string_agg(parent.rolname, ',') FROM pg_auth_members m JOIN pg_roles child ON child.oid=m.member JOIN pg_roles parent ON parent.oid=m.roleid WHERE child.rolname='baby_tracker_apple_revoker_app'")" = baby_tracker_apple_revoker
test "$(sql "SELECT count(*) FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.member WHERE r.rolname='baby_tracker_migrator'")" = 0

# Container smoke uses this positive-only mode. The exhaustive destructive
# mutation matrix remains available by default and is also covered by the real
# PostgreSQL Go tests, but repeatedly aborting the large bootstrap DO block can
# crash PostgreSQL 17 Alpine backends with SIGPIPE under docker exec.
if [ "$negative_probes" = 0 ]; then
  if [ "$(sql "SELECT to_regclass('public.schema_migrations') IS NOT NULL")" = t ]; then
    test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime', t, 'SELECT')) FROM unnest(ARRAY['public.schema_migrations','public.server_state','public.schema_compatibility','public.schema_privilege_manifest']) t")" = t
    test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_projector', t, 'SELECT')) FROM unnest(ARRAY['public.server_state','public.schema_compatibility','public.schema_privilege_manifest']) t")" = t
    test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime', t, 'SELECT') AND NOT has_table_privilege('baby_tracker_runtime', t, 'INSERT,UPDATE,DELETE,TRUNCATE')) FROM unnest(ARRAY['public.accounts','public.families','public.memberships']) t")" = t
	if [ "$(sql "SELECT current_version >= 9 FROM schema_compatibility WHERE singleton")" = t ]; then
	  test "$(sql "SELECT has_column_privilege('baby_tracker_runtime','public.families','next_sequence','UPDATE') AND NOT has_table_privilege('baby_tracker_runtime','public.families','UPDATE') AND NOT has_column_privilege('baby_tracker_runtime','public.families','status','UPDATE') AND NOT has_column_privilege('baby_tracker_runtime','public.families','display_name','UPDATE') AND NOT has_column_privilege('baby_tracker_runtime','public.families','purge_after','UPDATE')")" = t
	  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT,UPDATE') AND NOT has_table_privilege('baby_tracker_runtime',t,'DELETE,TRUNCATE')) FROM unnest(ARRAY['public.babies','public.feed_entries','public.sleep_entries','public.diaper_entries']) t")" = t
	  # 000027 dropped the feed side and food tables.
	  if [ "$(sql "SELECT current_version < 27 FROM schema_compatibility WHERE singleton")" = t ]; then
	    test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT,DELETE') AND NOT has_table_privilege('baby_tracker_runtime',t,'UPDATE,TRUNCATE')) FROM unnest(ARRAY['public.feed_side_intervals','public.feed_foods']) t")" = t
	  fi
	  test "$(sql "SELECT has_table_privilege('baby_tracker_runtime','public.mutation_receipts','SELECT') AND NOT has_table_privilege('baby_tracker_runtime','public.mutation_receipts','INSERT,UPDATE,DELETE,TRUNCATE') AND has_table_privilege('baby_tracker_runtime','public.mutation_journal','SELECT,INSERT') AND NOT has_table_privilege('baby_tracker_runtime','public.mutation_journal','UPDATE,DELETE,TRUNCATE')")" = t
	  test "$(sql "SELECT has_table_privilege('baby_tracker_runtime','public.family_record_usage','SELECT,INSERT,UPDATE') AND NOT has_table_privilege('baby_tracker_runtime','public.family_record_usage','DELETE,TRUNCATE')")" = t
	  test "$(sql "SELECT has_function_privilege('baby_tracker_runtime','app.save_mutation_receipt(uuid,uuid,uuid,uuid,bytea,jsonb,timestamp with time zone,bigint)','EXECUTE')")" = t
	fi
	if [ "$(sql "SELECT current_version >= 10 FROM schema_compatibility WHERE singleton")" = t ]; then
	  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT') AND NOT has_table_privilege('baby_tracker_runtime',t,'UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public.sync_page_sessions','public.sync_snapshots','public.sync_snapshot_entities']) t")" = t
	  test "$(sql "SELECT NOT has_table_privilege('baby_tracker_runtime','public.sync_snapshot_admissions','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')")" = t
	  test "$(sql "SELECT has_function_privilege('baby_tracker_runtime','app.sync_snapshot_admission_counts(uuid,uuid,timestamp with time zone)','EXECUTE') AND NOT has_function_privilege('baby_tracker_runtime','app.stage_sync_snapshot_admission()','EXECUTE')")" = t
	  test "$(sql "SELECT NOT bool_or(has_table_privilege('baby_tracker_projector',t,p)) FROM unnest(ARRAY['public.sync_page_sessions','public.sync_snapshots','public.sync_snapshot_entities','public.sync_snapshot_admissions']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
	fi
		if [ "$(sql "SELECT current_version >= 11 FROM schema_compatibility WHERE singleton")" = t ]; then
		  test "$(sql "SELECT has_table_privilege('baby_tracker_runtime','public.active_timers','SELECT,INSERT,UPDATE') AND NOT has_table_privilege('baby_tracker_runtime','public.active_timers','DELETE,TRUNCATE,REFERENCES,TRIGGER')")" = t
		  test "$(sql "SELECT NOT bool_or(has_table_privilege(r,t,p)) FROM unnest(ARRAY['public','baby_tracker_projector','baby_tracker_apple_revoker']) r CROSS JOIN unnest(ARRAY['public.active_timers']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  # 000027 dropped the timer side table.
		  if [ "$(sql "SELECT current_version < 27 FROM schema_compatibility WHERE singleton")" = t ]; then
		    test "$(sql "SELECT has_table_privilege('baby_tracker_runtime','public.timer_side_intervals','SELECT,INSERT,DELETE') AND NOT has_table_privilege('baby_tracker_runtime','public.timer_side_intervals','UPDATE,TRUNCATE,REFERENCES,TRIGGER')")" = t
		    test "$(sql "SELECT NOT bool_or(has_table_privilege(r,t,p)) FROM unnest(ARRAY['public','baby_tracker_projector','baby_tracker_apple_revoker']) r CROSS JOIN unnest(ARRAY['public.timer_side_intervals']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  fi
		fi
		if [ "$(sql "SELECT current_version >= 12 FROM schema_compatibility WHERE singleton")" = t ]; then
		  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT,UPDATE') AND NOT has_table_privilege('baby_tracker_runtime',t,'DELETE,TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public.import_sessions','public.import_conflicts']) t")" = t
		  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT') AND NOT has_table_privilege('baby_tracker_runtime',t,'UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public.import_chunks','public.import_baby_candidates','public.import_record_manifests','public.import_conflict_receipts','public.import_baby_mappings']) t")" = t
		  test "$(sql "SELECT NOT bool_or(has_table_privilege(r,t,p)) FROM unnest(ARRAY['public','baby_tracker_projector','baby_tracker_apple_revoker']) r CROSS JOIN unnest(ARRAY['public.import_sessions','public.import_chunks','public.import_baby_candidates','public.import_record_manifests','public.import_conflicts','public.import_conflict_receipts','public.import_baby_mappings']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  test "$(sql "SELECT has_function_privilege('baby_tracker_runtime','app.import_unfinished_session_count(uuid,timestamp with time zone)','EXECUTE') AND NOT has_function_privilege('public','app.import_unfinished_session_count(uuid,timestamp with time zone)','EXECUTE') AND NOT has_function_privilege('baby_tracker_projector','app.import_unfinished_session_count(uuid,timestamp with time zone)','EXECUTE') AND NOT has_function_privilege('baby_tracker_apple_revoker','app.import_unfinished_session_count(uuid,timestamp with time zone)','EXECUTE')")" = t
		fi
		if [ "$(sql "SELECT current_version >= 13 FROM schema_compatibility WHERE singleton")" = t ]; then
		  test "$(sql "SELECT NOT bool_or(has_table_privilege(r,'public.delivery_pending',p)) FROM unnest(ARRAY['public','baby_tracker_runtime','baby_tracker_projector','baby_tracker_apple_revoker']) r CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  test "$(sql "SELECT has_function_privilege('baby_tracker_runtime','app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)','EXECUTE') AND NOT has_function_privilege('public','app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)','EXECUTE') AND NOT has_function_privilege('baby_tracker_projector','app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)','EXECUTE') AND NOT has_function_privilege('baby_tracker_apple_revoker','app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)','EXECUTE')")" = t
		fi
    test "$(sql "SELECT has_schema_privilege('baby_tracker_runtime', 'app', 'USAGE') AND has_schema_privilege('baby_tracker_runtime', 'public', 'USAGE') AND has_function_privilege('baby_tracker_runtime', 'app.current_account_id()', 'EXECUTE') AND has_function_privilege('baby_tracker_runtime', 'app.current_family_id()', 'EXECUTE')")" = t
    test "$(sql "SELECT has_schema_privilege('baby_tracker_apple_revoker_app','app','USAGE')")" = t
    test "$(sql "SELECT bool_and(has_function_privilege('baby_tracker_apple_revoker_app', object_identity, 'EXECUTE')) FROM schema_privilege_manifest WHERE schema_version=(SELECT current_version FROM schema_compatibility WHERE singleton) AND principal='baby_tracker_apple_revoker' AND object_kind='function'")" = t
    test "$(sql "SELECT NOT bool_or(has_table_privilege('baby_tracker_apple_revoker_app', t, p)) FROM unnest(ARRAY['public.apple_revocation_jobs','public.credential_cleanup_jobs','public.account_identities','public.auth_exchange_material']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE']) p")" = t
    test "$(sql "SELECT NOT has_function_privilege('baby_tracker_app','app.claim_apple_revocation_job(text,timestamp with time zone,timestamp with time zone)','EXECUTE') AND NOT has_function_privilege('baby_tracker_projector_app','app.claim_apple_revocation_job(text,timestamp with time zone,timestamp with time zone)','EXECUTE')")" = t
  fi
  echo 'principal proof: positive role and exact current privilege contract verified; destructive probes delegated to real-PostgreSQL tests'
  exit 0
fi
for principal in baby_tracker_migrator baby_tracker_app baby_tracker_export_app baby_tracker_projector_app baby_tracker_apple_revoker_app; do
  for attribute in SUPERUSER BYPASSRLS CREATEDB CREATEROLE REPLICATION; do
    sql "ALTER ROLE $principal $attribute" >/dev/null
    if bootstrap >/dev/null 2>&1; then
      echo "bootstrap accepted unsafe principal attribute: $principal $attribute" >&2
      exit 1
    fi
    sql "ALTER ROLE $principal NO$attribute" >/dev/null
  done
  sql "ALTER ROLE $principal NOLOGIN" >/dev/null
  if bootstrap >/dev/null 2>&1; then exit 1; fi
  sql "ALTER ROLE $principal LOGIN" >/dev/null
  sql "ALTER ROLE $principal NOINHERIT" >/dev/null
  if bootstrap >/dev/null 2>&1; then exit 1; fi
  sql "ALTER ROLE $principal INHERIT" >/dev/null
  sql "ALTER ROLE $principal SET search_path = pg_catalog" >/dev/null
  if bootstrap >/dev/null 2>&1; then exit 1; fi
  sql "ALTER ROLE $principal RESET ALL" >/dev/null
  sql "ALTER ROLE $principal VALID UNTIL '2000-01-01'" >/dev/null
  if bootstrap >/dev/null 2>&1; then exit 1; fi
  sql "ALTER ROLE $principal VALID UNTIL 'infinity'" >/dev/null
  unexpected_group=baby_tracker_maintenance
  if [ "$principal" = baby_tracker_export_app ]; then unexpected_group=baby_tracker_runtime; fi
  sql "GRANT $unexpected_group TO $principal" >/dev/null
  if bootstrap >/dev/null 2>&1; then exit 1; fi
  sql "REVOKE $unexpected_group FROM $principal" >/dev/null
done
sql 'GRANT baby_tracker_runtime TO baby_tracker_app WITH ADMIN TRUE' >/dev/null
if bootstrap >/dev/null 2>&1; then exit 1; fi
sql 'REVOKE ADMIN OPTION FOR baby_tracker_runtime FROM baby_tracker_app' >/dev/null
sql 'GRANT baby_tracker_runtime TO baby_tracker_app WITH INHERIT FALSE, SET FALSE' >/dev/null
if bootstrap >/dev/null 2>&1; then exit 1; fi
sql 'GRANT baby_tracker_runtime TO baby_tracker_app WITH INHERIT TRUE, SET TRUE' >/dev/null
bootstrap
echo 'principal proof: compatible reruns succeed; unsafe attributes, memberships and admin option rejected'

# Exercise catalog state, not effective privileges: PUBLIC/group ACL entries are
# compatible; direct runtime escalation and runtime ownership must fail closed.
# Accumulate failures so the RED run demonstrates each independent gap.
failures=0
reject_catalog_state() {
  label=$1 setup=$2 retained=$3 restore=$4
  sql "$setup" >/dev/null
  if bootstrap >/dev/null 2>&1; then
    echo "FAIL: bootstrap accepted $label" >&2
    failures=$((failures + 1))
  elif [ "$(sql "$retained")" != t ]; then
    echo "FAIL: bootstrap silently changed $label" >&2
    failures=$((failures + 1))
  else
    echo "catalog proof: rejected and preserved $label"
  fi
  sql "$restore" >/dev/null
}

sql 'CREATE SCHEMA bootstrap_probe AUTHORIZATION baby_tracker_migrator;
     CREATE TABLE bootstrap_probe.acl_probe (id integer);
     ALTER TABLE bootstrap_probe.acl_probe OWNER TO baby_tracker_migrator' >/dev/null
for schema in public bootstrap_probe; do
  reject_catalog_state "direct CREATE on schema $schema" \
    "GRANT CREATE ON SCHEMA $schema TO baby_tracker_app" \
    "SELECT has_schema_privilege('baby_tracker_app', '$schema', 'CREATE')" \
    "REVOKE CREATE ON SCHEMA $schema FROM baby_tracker_app"
done
for privilege in CREATE TEMPORARY; do
  reject_catalog_state "direct database $privilege" \
    "GRANT $privilege ON DATABASE babytracker TO baby_tracker_app" \
    "SELECT has_database_privilege('baby_tracker_app', 'babytracker', '$privilege')" \
    "REVOKE $privilege ON DATABASE babytracker FROM baby_tracker_app"
done
for privilege in INSERT UPDATE DELETE TRUNCATE REFERENCES TRIGGER MAINTAIN; do
  reject_catalog_state "direct table $privilege" \
    "GRANT $privilege ON bootstrap_probe.acl_probe TO baby_tracker_app" \
    "SELECT has_table_privilege('baby_tracker_app', 'bootstrap_probe.acl_probe', '$privilege')" \
    "REVOKE $privilege ON bootstrap_probe.acl_probe FROM baby_tracker_app"
done
for privilege in INSERT UPDATE REFERENCES; do
  reject_catalog_state "direct column $privilege" \
    "GRANT $privilege (id) ON bootstrap_probe.acl_probe TO baby_tracker_app" \
    "SELECT has_column_privilege('baby_tracker_app', 'bootstrap_probe.acl_probe', 'id', '$privilege')" \
    "REVOKE $privilege (id) ON bootstrap_probe.acl_probe FROM baby_tracker_app"
done
reject_catalog_state 'runtime-owned database' \
  'ALTER DATABASE babytracker OWNER TO baby_tracker_app' \
  "SELECT datdba = 'baby_tracker_app'::regrole FROM pg_database WHERE datname = 'babytracker'" \
  'ALTER DATABASE babytracker OWNER TO baby_tracker_migrator'
for schema in public bootstrap_probe; do
  reject_catalog_state "runtime-owned schema $schema" \
    "ALTER SCHEMA $schema OWNER TO baby_tracker_app" \
    "SELECT nspowner = 'baby_tracker_app'::regrole FROM pg_namespace WHERE nspname = '$schema'" \
    "ALTER SCHEMA $schema OWNER TO baby_tracker_migrator"
done
reject_catalog_state 'runtime-owned persistent table' \
  'ALTER TABLE bootstrap_probe.acl_probe OWNER TO baby_tracker_app' \
  "SELECT relowner = 'baby_tracker_app'::regrole FROM pg_class WHERE oid = 'bootstrap_probe.acl_probe'::regclass" \
  'ALTER TABLE bootstrap_probe.acl_probe OWNER TO baby_tracker_migrator'
reject_catalog_state 'runtime-owned sequence' \
  'CREATE SEQUENCE bootstrap_probe.owned_sequence; ALTER SEQUENCE bootstrap_probe.owned_sequence OWNER TO baby_tracker_app' \
  "SELECT relowner = 'baby_tracker_app'::regrole FROM pg_class WHERE oid = 'bootstrap_probe.owned_sequence'::regclass" \
  'DROP SEQUENCE bootstrap_probe.owned_sequence'
reject_catalog_state 'runtime-owned function' \
  "CREATE FUNCTION bootstrap_probe.owned_function() RETURNS integer LANGUAGE sql AS 'SELECT 1'; ALTER FUNCTION bootstrap_probe.owned_function() OWNER TO baby_tracker_app" \
  "SELECT proowner = 'baby_tracker_app'::regrole FROM pg_proc WHERE oid = 'bootstrap_probe.owned_function()'::regprocedure" \
  'DROP FUNCTION bootstrap_probe.owned_function()'
reject_catalog_state 'runtime-owned type' \
  "CREATE TYPE bootstrap_probe.owned_type AS ENUM ('test'); ALTER TYPE bootstrap_probe.owned_type OWNER TO baby_tracker_app" \
  "SELECT typowner = 'baby_tracker_app'::regrole FROM pg_type WHERE oid = 'bootstrap_probe.owned_type'::regtype" \
  'DROP TYPE bootstrap_probe.owned_type'

# The LOGIN inherits this group's authority. Catalog grants to the group must
# be checked separately from allowed tenant DML and unrelated PUBLIC grants.
for schema in public bootstrap_probe; do
  reject_catalog_state "group CREATE on schema $schema" \
    "GRANT CREATE ON SCHEMA $schema TO baby_tracker_runtime" \
    "SELECT has_schema_privilege('baby_tracker_runtime', '$schema', 'CREATE')" \
    "REVOKE CREATE ON SCHEMA $schema FROM baby_tracker_runtime"
done
for privilege in CREATE TEMPORARY; do
  reject_catalog_state "group database $privilege" \
    "GRANT $privilege ON DATABASE babytracker TO baby_tracker_runtime" \
    "SELECT has_database_privilege('baby_tracker_runtime', 'babytracker', '$privilege')" \
    "REVOKE $privilege ON DATABASE babytracker FROM baby_tracker_runtime"
done
for privilege in TRUNCATE REFERENCES TRIGGER MAINTAIN; do
  reject_catalog_state "group table $privilege" \
    "GRANT $privilege ON bootstrap_probe.acl_probe TO baby_tracker_runtime" \
    "SELECT has_table_privilege('baby_tracker_runtime', 'bootstrap_probe.acl_probe', '$privilege')" \
    "REVOKE $privilege ON bootstrap_probe.acl_probe FROM baby_tracker_runtime"
done
reject_catalog_state 'group column REFERENCES' \
  'GRANT REFERENCES (id) ON bootstrap_probe.acl_probe TO baby_tracker_runtime' \
  "SELECT has_column_privilege('baby_tracker_runtime', 'bootstrap_probe.acl_probe', 'id', 'REFERENCES')" \
  'REVOKE REFERENCES (id) ON bootstrap_probe.acl_probe FROM baby_tracker_runtime'
reject_catalog_state 'group-owned database' \
  'ALTER DATABASE babytracker OWNER TO baby_tracker_runtime' \
  "SELECT datdba = 'baby_tracker_runtime'::regrole FROM pg_database WHERE datname = 'babytracker'" \
  'ALTER DATABASE babytracker OWNER TO baby_tracker_migrator'
for schema in public bootstrap_probe; do
  reject_catalog_state "group-owned schema $schema" \
    "ALTER SCHEMA $schema OWNER TO baby_tracker_runtime" \
    "SELECT nspowner = 'baby_tracker_runtime'::regrole FROM pg_namespace WHERE nspname = '$schema'" \
    "ALTER SCHEMA $schema OWNER TO baby_tracker_migrator"
done
reject_catalog_state 'group-owned persistent table' \
  'ALTER TABLE bootstrap_probe.acl_probe OWNER TO baby_tracker_runtime' \
  "SELECT relowner = 'baby_tracker_runtime'::regrole FROM pg_class WHERE oid = 'bootstrap_probe.acl_probe'::regclass" \
  'ALTER TABLE bootstrap_probe.acl_probe OWNER TO baby_tracker_migrator'
reject_catalog_state 'group-owned sequence' \
  'CREATE SEQUENCE bootstrap_probe.owned_sequence; ALTER SEQUENCE bootstrap_probe.owned_sequence OWNER TO baby_tracker_runtime' \
  "SELECT relowner = 'baby_tracker_runtime'::regrole FROM pg_class WHERE oid = 'bootstrap_probe.owned_sequence'::regclass" \
  'DROP SEQUENCE bootstrap_probe.owned_sequence'
reject_catalog_state 'group-owned function' \
  "CREATE FUNCTION bootstrap_probe.owned_function() RETURNS integer LANGUAGE sql AS 'SELECT 1'; ALTER FUNCTION bootstrap_probe.owned_function() OWNER TO baby_tracker_runtime" \
  "SELECT proowner = 'baby_tracker_runtime'::regrole FROM pg_proc WHERE oid = 'bootstrap_probe.owned_function()'::regprocedure" \
  'DROP FUNCTION bootstrap_probe.owned_function()'
reject_catalog_state 'group-owned type' \
  "CREATE TYPE bootstrap_probe.owned_type AS ENUM ('test'); ALTER TYPE bootstrap_probe.owned_type OWNER TO baby_tracker_runtime" \
  "SELECT typowner = 'baby_tracker_runtime'::regrole FROM pg_type WHERE oid = 'bootstrap_probe.owned_type'::regtype" \
  'DROP TYPE bootstrap_probe.owned_type'

for table in schema_migrations server_state schema_compatibility schema_privilege_manifest; do
  if [ "$(sql "SELECT to_regclass('public.$table') IS NOT NULL")" = t ]; then
    for privilege in INSERT UPDATE DELETE TRUNCATE REFERENCES TRIGGER MAINTAIN; do
      reject_catalog_state "group operational $table $privilege" \
        "GRANT $privilege ON public.$table TO baby_tracker_runtime" \
        "SELECT has_table_privilege('baby_tracker_runtime', 'public.$table', '$privilege')" \
        "REVOKE $privilege ON public.$table FROM baby_tracker_runtime"
    done
    column=$(sql "SELECT attname FROM pg_attribute WHERE attrelid = 'public.$table'::regclass AND attnum = 1")
    for privilege in INSERT UPDATE REFERENCES; do
      reject_catalog_state "group operational column $table $privilege" \
        "GRANT $privilege ($column) ON public.$table TO baby_tracker_runtime" \
        "SELECT has_column_privilege('baby_tracker_runtime', 'public.$table', '$column', '$privilege')" \
        "REVOKE $privilege ($column) ON public.$table FROM baby_tracker_runtime"
    done
  fi
done

# A compatible rerun preserves only the explicit post-migration allowlist.
bootstrap >/dev/null
test "$(sql "SELECT pg_has_role('baby_tracker_app', 'baby_tracker_runtime', 'MEMBER')")" = t
test "$(sql "SELECT pg_has_role('baby_tracker_export_app', 'baby_tracker_maintenance', 'MEMBER')")" = t
test "$(sql "SELECT pg_has_role('baby_tracker_apple_revoker_app', 'baby_tracker_apple_revoker', 'MEMBER')")" = t
if [ "$(sql "SELECT to_regclass('public.schema_migrations') IS NOT NULL")" = t ]; then
  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime', t, 'SELECT')) FROM unnest(ARRAY['public.schema_migrations','public.server_state','public.schema_compatibility','public.schema_privilege_manifest']) t")" = t
  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_projector', t, 'SELECT')) FROM unnest(ARRAY['public.server_state','public.schema_compatibility','public.schema_privilege_manifest']) t")" = t
  if [ "$(sql "SELECT current_version >= 6 FROM schema_compatibility WHERE singleton")" = t ]; then
    test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime', t, 'SELECT') AND NOT has_table_privilege('baby_tracker_runtime', t, 'INSERT,UPDATE,DELETE,TRUNCATE')) FROM unnest(ARRAY['public.accounts','public.families','public.memberships']) t")" = t
  else
    test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime', t, 'SELECT') AND has_table_privilege('baby_tracker_runtime', t, 'INSERT') AND has_table_privilege('baby_tracker_runtime', t, 'UPDATE') AND has_table_privilege('baby_tracker_runtime', t, 'DELETE')) FROM unnest(ARRAY['public.accounts','public.families','public.memberships']) t")" = t
  fi
  test "$(sql "SELECT has_schema_privilege('baby_tracker_runtime', 'app', 'USAGE') AND has_schema_privilege('baby_tracker_runtime', 'public', 'USAGE') AND has_function_privilege('baby_tracker_runtime', 'app.current_account_id()', 'EXECUTE') AND has_function_privilege('baby_tracker_runtime', 'app.current_family_id()', 'EXECUTE')")" = t

  reject_catalog_state 'projector group tenant UPDATE on families' \
    'GRANT UPDATE ON public.families TO baby_tracker_projector' \
    "SELECT has_table_privilege('baby_tracker_projector_app', 'public.families', 'UPDATE')" \
    'REVOKE UPDATE ON public.families FROM baby_tracker_projector'
  reject_catalog_state 'projector group tenant DELETE on memberships' \
    'GRANT DELETE ON public.memberships TO baby_tracker_projector' \
    "SELECT has_table_privilege('baby_tracker_projector_app', 'public.memberships', 'DELETE')" \
    'REVOKE DELETE ON public.memberships FROM baby_tracker_projector'
  reject_catalog_state 'projector group tenant context helper' \
    'GRANT EXECUTE ON FUNCTION app.current_family_id() TO baby_tracker_projector' \
    "SELECT has_function_privilege('baby_tracker_projector_app', 'app.current_family_id()', 'EXECUTE')" \
    'REVOKE EXECUTE ON FUNCTION app.current_family_id() FROM baby_tracker_projector'

  if [ "$(sql "SELECT current_version >= 7 FROM schema_compatibility WHERE singleton")" = t ]; then
    test "$(sql "SELECT has_schema_privilege('baby_tracker_apple_revoker_app','app','USAGE')")" = t
    test "$(sql "SELECT bool_and(has_function_privilege('baby_tracker_apple_revoker_app', object_identity, 'EXECUTE')) FROM schema_privilege_manifest WHERE schema_version=(SELECT current_version FROM schema_compatibility WHERE singleton) AND principal='baby_tracker_apple_revoker' AND object_kind='function'")" = t
    test "$(sql "SELECT NOT bool_or(has_table_privilege('baby_tracker_apple_revoker_app', t, p)) FROM unnest(ARRAY['public.apple_revocation_jobs','public.credential_cleanup_jobs','public.account_identities','public.auth_exchange_material']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE']) p")" = t
    test "$(sql "SELECT NOT has_function_privilege('baby_tracker_app','app.claim_apple_revocation_job(text,timestamp with time zone,timestamp with time zone)','EXECUTE') AND NOT has_function_privilege('baby_tracker_projector_app','app.claim_apple_revocation_job(text,timestamp with time zone,timestamp with time zone)','EXECUTE')")" = t
  fi
	if [ "$(sql "SELECT current_version >= 9 FROM schema_compatibility WHERE singleton")" = t ]; then
	  test "$(sql "SELECT has_column_privilege('baby_tracker_runtime','public.families','next_sequence','UPDATE') AND NOT has_table_privilege('baby_tracker_runtime','public.families','UPDATE') AND NOT has_column_privilege('baby_tracker_runtime','public.families','status','UPDATE') AND NOT has_column_privilege('baby_tracker_runtime','public.families','display_name','UPDATE') AND NOT has_column_privilege('baby_tracker_runtime','public.families','purge_after','UPDATE')")" = t
	  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT,UPDATE') AND NOT has_table_privilege('baby_tracker_runtime',t,'DELETE,TRUNCATE')) FROM unnest(ARRAY['public.babies','public.feed_entries','public.sleep_entries','public.diaper_entries']) t")" = t
	  # 000027 dropped the feed side and food tables.
	  if [ "$(sql "SELECT current_version < 27 FROM schema_compatibility WHERE singleton")" = t ]; then
	    test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT,DELETE') AND NOT has_table_privilege('baby_tracker_runtime',t,'UPDATE,TRUNCATE')) FROM unnest(ARRAY['public.feed_side_intervals','public.feed_foods']) t")" = t
	  fi
	  test "$(sql "SELECT has_table_privilege('baby_tracker_runtime','public.mutation_receipts','SELECT') AND NOT has_table_privilege('baby_tracker_runtime','public.mutation_receipts','INSERT,UPDATE,DELETE,TRUNCATE') AND has_table_privilege('baby_tracker_runtime','public.mutation_journal','SELECT,INSERT') AND NOT has_table_privilege('baby_tracker_runtime','public.mutation_journal','UPDATE,DELETE,TRUNCATE')")" = t
	  test "$(sql "SELECT has_table_privilege('baby_tracker_runtime','public.family_record_usage','SELECT,INSERT,UPDATE') AND NOT has_table_privilege('baby_tracker_runtime','public.family_record_usage','DELETE,TRUNCATE')")" = t
	  test "$(sql "SELECT has_function_privilege('baby_tracker_runtime','app.save_mutation_receipt(uuid,uuid,uuid,uuid,bytea,jsonb,timestamp with time zone,bigint)','EXECUTE')")" = t
	fi
	if [ "$(sql "SELECT current_version >= 10 FROM schema_compatibility WHERE singleton")" = t ]; then
	  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT') AND NOT has_table_privilege('baby_tracker_runtime',t,'UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public.sync_page_sessions','public.sync_snapshots','public.sync_snapshot_entities']) t")" = t
	  test "$(sql "SELECT NOT has_table_privilege('baby_tracker_runtime','public.sync_snapshot_admissions','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')")" = t
	  test "$(sql "SELECT has_function_privilege('baby_tracker_runtime','app.sync_snapshot_admission_counts(uuid,uuid,timestamp with time zone)','EXECUTE') AND NOT has_function_privilege('baby_tracker_runtime','app.stage_sync_snapshot_admission()','EXECUTE')")" = t
	  test "$(sql "SELECT NOT bool_or(has_table_privilege('baby_tracker_projector',t,p)) FROM unnest(ARRAY['public.sync_page_sessions','public.sync_snapshots','public.sync_snapshot_entities','public.sync_snapshot_admissions']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
	fi
		if [ "$(sql "SELECT current_version >= 11 FROM schema_compatibility WHERE singleton")" = t ]; then
		  test "$(sql "SELECT has_table_privilege('baby_tracker_runtime','public.active_timers','SELECT,INSERT,UPDATE') AND NOT has_table_privilege('baby_tracker_runtime','public.active_timers','DELETE,TRUNCATE,REFERENCES,TRIGGER')")" = t
		  test "$(sql "SELECT NOT bool_or(has_table_privilege(r,t,p)) FROM unnest(ARRAY['public','baby_tracker_projector','baby_tracker_apple_revoker']) r CROSS JOIN unnest(ARRAY['public.active_timers']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  # 000027 dropped the timer side table.
		  if [ "$(sql "SELECT current_version < 27 FROM schema_compatibility WHERE singleton")" = t ]; then
		    test "$(sql "SELECT has_table_privilege('baby_tracker_runtime','public.timer_side_intervals','SELECT,INSERT,DELETE') AND NOT has_table_privilege('baby_tracker_runtime','public.timer_side_intervals','UPDATE,TRUNCATE,REFERENCES,TRIGGER')")" = t
		    test "$(sql "SELECT NOT bool_or(has_table_privilege(r,t,p)) FROM unnest(ARRAY['public','baby_tracker_projector','baby_tracker_apple_revoker']) r CROSS JOIN unnest(ARRAY['public.timer_side_intervals']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  fi
		fi
		if [ "$(sql "SELECT current_version >= 12 FROM schema_compatibility WHERE singleton")" = t ]; then
		  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT,UPDATE') AND NOT has_table_privilege('baby_tracker_runtime',t,'DELETE,TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public.import_sessions','public.import_conflicts']) t")" = t
		  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT') AND NOT has_table_privilege('baby_tracker_runtime',t,'UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public.import_chunks','public.import_baby_candidates','public.import_record_manifests','public.import_conflict_receipts','public.import_baby_mappings']) t")" = t
		  test "$(sql "SELECT NOT bool_or(has_table_privilege(r,t,p)) FROM unnest(ARRAY['public','baby_tracker_projector','baby_tracker_apple_revoker']) r CROSS JOIN unnest(ARRAY['public.import_sessions','public.import_chunks','public.import_baby_candidates','public.import_record_manifests','public.import_conflicts','public.import_conflict_receipts','public.import_baby_mappings']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  test "$(sql "SELECT has_function_privilege('baby_tracker_runtime','app.import_unfinished_session_count(uuid,timestamp with time zone)','EXECUTE') AND NOT has_function_privilege('public','app.import_unfinished_session_count(uuid,timestamp with time zone)','EXECUTE') AND NOT has_function_privilege('baby_tracker_projector','app.import_unfinished_session_count(uuid,timestamp with time zone)','EXECUTE') AND NOT has_function_privilege('baby_tracker_apple_revoker','app.import_unfinished_session_count(uuid,timestamp with time zone)','EXECUTE')")" = t
		fi
		if [ "$(sql "SELECT current_version >= 13 FROM schema_compatibility WHERE singleton")" = t ]; then
		  test "$(sql "SELECT NOT bool_or(has_table_privilege(r,'public.delivery_pending',p)) FROM unnest(ARRAY['public','baby_tracker_runtime','baby_tracker_projector','baby_tracker_apple_revoker']) r CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  test "$(sql "SELECT has_function_privilege('baby_tracker_runtime','app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)','EXECUTE') AND NOT has_function_privilege('public','app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)','EXECUTE') AND NOT has_function_privilege('baby_tracker_projector','app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)','EXECUTE') AND NOT has_function_privilege('baby_tracker_apple_revoker','app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)','EXECUTE')")" = t
		fi
fi
sql 'DROP TABLE bootstrap_probe.acl_probe; DROP SCHEMA bootstrap_probe' >/dev/null
bootstrap >/dev/null
if [ "$failures" -ne 0 ]; then
  echo "catalog proof FAILED: $failures incompatible states accepted" >&2
  exit 1
fi
echo 'catalog proof: login/group ACL and ownership attacks rejected; compatible tenant DML, operational SELECT, schema USAGE, function EXECUTE and incoming membership preserved'
