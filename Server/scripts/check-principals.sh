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
test "$(sql "SELECT count(*) FROM pg_roles WHERE rolname IN ('baby_tracker_migrator','baby_tracker_app','baby_tracker_export_app') AND rolcanlogin AND NOT (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)")" = 3
# The bootstrap's roles are exactly these five: the projector and Apple-revoker
# roles and logins are gone (Phase 4 Task 27).
test "$(sql "SELECT string_agg(rolname, ',' ORDER BY rolname) FROM pg_roles WHERE rolname LIKE 'baby\_tracker\_%'")" = baby_tracker_app,baby_tracker_export_app,baby_tracker_maintenance,baby_tracker_migrator,baby_tracker_runtime
test "$(sql "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='babytracker'")" = baby_tracker_migrator
test "$(sql "SELECT string_agg(parent.rolname, ',') FROM pg_auth_members m JOIN pg_roles child ON child.oid=m.member JOIN pg_roles parent ON parent.oid=m.roleid WHERE child.rolname='baby_tracker_app'")" = baby_tracker_runtime
test "$(sql "SELECT string_agg(parent.rolname, ',') FROM pg_auth_members m JOIN pg_roles child ON child.oid=m.member JOIN pg_roles parent ON parent.oid=m.roleid WHERE child.rolname='baby_tracker_export_app'")" = baby_tracker_maintenance
test "$(sql "SELECT count(*) FROM pg_auth_members m JOIN pg_roles r ON r.oid=m.member WHERE r.rolname='baby_tracker_migrator'")" = 0

# Container smoke uses this positive-only mode. The exhaustive destructive
# mutation matrix remains available by default and is also covered by the real
# PostgreSQL Go tests, but repeatedly aborting the large bootstrap DO block can
# crash PostgreSQL 17 Alpine backends with SIGPIPE under docker exec.
if [ "$negative_probes" = 0 ]; then
  if [ "$(sql "SELECT to_regclass('public.schema_migrations') IS NOT NULL")" = t ]; then
    test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime', t, 'SELECT')) FROM unnest(ARRAY['public.schema_migrations','public.server_state','public.schema_compatibility','public.schema_privilege_manifest']) t")" = t
    # Schema 29 (PostgreSQL authority): the authority writes accounts,
    # families and memberships itself (INSERT plus named UPDATE columns,
    # never a table-wide UPDATE, DELETE or TRUNCATE); server_state is written
    # only by `baby-tracker-admin authority init|rotate-epoch` (migrator).
    authority=$(sql "SELECT current_version >= 29 FROM schema_compatibility WHERE singleton")
    if [ "$authority" = t ]; then
      test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime', t, 'SELECT,INSERT') AND NOT has_table_privilege('baby_tracker_runtime', t, 'UPDATE') AND NOT has_table_privilege('baby_tracker_runtime', t, 'DELETE') AND NOT has_table_privilege('baby_tracker_runtime', t, 'TRUNCATE')) FROM unnest(ARRAY['public.accounts','public.families','public.memberships']) t")" = t
      test "$(sql "SELECT NOT has_table_privilege('baby_tracker_runtime', 'public.server_state', 'INSERT') AND NOT has_table_privilege('baby_tracker_runtime', 'public.server_state', 'UPDATE') AND NOT has_table_privilege('baby_tracker_runtime', 'public.server_state', 'DELETE')")" = t
      test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime', t, 'SELECT') AND has_table_privilege('baby_tracker_runtime', t, 'INSERT') AND NOT has_table_privilege('baby_tracker_runtime', t, 'UPDATE') AND NOT has_table_privilege('baby_tracker_runtime', t, 'DELETE') AND NOT has_table_privilege('baby_tracker_runtime', t, 'TRUNCATE')) FROM unnest(ARRAY['public.security_audit_events','public.command_receipts']) t")" = t
      test "$(sql "SELECT NOT bool_or(has_table_privilege(r, t, p)) FROM unnest(ARRAY['baby_tracker_maintenance','public']) r CROSS JOIN unnest(ARRAY['public.sessions','public.account_identities','public.security_audit_events','public.command_receipts','public.mcp_integrations']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE']) p")" = t
    else
      test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime', t, 'SELECT') AND NOT has_table_privilege('baby_tracker_runtime', t, 'INSERT,UPDATE,DELETE,TRUNCATE')) FROM unnest(ARRAY['public.accounts','public.families','public.memberships']) t")" = t
    fi
	if [ "$authority" = t ]; then
	  test "$(sql "SELECT has_column_privilege('baby_tracker_runtime','public.families','next_sequence','UPDATE') AND NOT has_table_privilege('baby_tracker_runtime','public.families','UPDATE') AND NOT has_column_privilege('baby_tracker_runtime','public.families','id','UPDATE') AND NOT has_column_privilege('baby_tracker_runtime','public.families','created_at','UPDATE')")" = t
	fi
	if [ "$(sql "SELECT current_version >= 9 FROM schema_compatibility WHERE singleton")" = t ] && [ "$authority" != t ]; then
	  test "$(sql "SELECT has_column_privilege('baby_tracker_runtime','public.families','next_sequence','UPDATE') AND NOT has_table_privilege('baby_tracker_runtime','public.families','UPDATE') AND NOT has_column_privilege('baby_tracker_runtime','public.families','status','UPDATE') AND NOT has_column_privilege('baby_tracker_runtime','public.families','display_name','UPDATE') AND NOT has_column_privilege('baby_tracker_runtime','public.families','purge_after','UPDATE')")" = t
	fi
		if [ "$(sql "SELECT current_version >= 12 FROM schema_compatibility WHERE singleton")" = t ]; then
		  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT,UPDATE') AND NOT has_table_privilege('baby_tracker_runtime',t,'DELETE,TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public.import_sessions','public.import_conflicts']) t")" = t
		  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT') AND NOT has_table_privilege('baby_tracker_runtime',t,'UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public.import_chunks','public.import_baby_candidates','public.import_record_manifests','public.import_conflict_receipts','public.import_baby_mappings']) t")" = t
		  test "$(sql "SELECT NOT bool_or(has_table_privilege(r,t,p)) FROM unnest(ARRAY['public','baby_tracker_maintenance']) r CROSS JOIN unnest(ARRAY['public.import_sessions','public.import_chunks','public.import_baby_candidates','public.import_record_manifests','public.import_conflicts','public.import_conflict_receipts','public.import_baby_mappings']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  test "$(sql "SELECT has_function_privilege('baby_tracker_runtime','app.import_unfinished_session_count(uuid,timestamp with time zone)','EXECUTE') AND NOT has_function_privilege('public','app.import_unfinished_session_count(uuid,timestamp with time zone)','EXECUTE') AND NOT has_function_privilege('baby_tracker_maintenance','app.import_unfinished_session_count(uuid,timestamp with time zone)','EXECUTE')")" = t
		fi
		# Schema 30 (Phase 2): care data lives in care_entities, beside its
		# receipts, continuation bindings and snapshot sessions. The runtime reads
		# and inserts them and updates only care_entities' named columns and the
		# family usage counters; PUBLIC never reaches them (the maintenance
		# group's retention grants are schema 31's, below). has_table_privilege sees only table-level
		# grants, so column-level grants are checked with
		# has_any_column_privilege and the exact care_entities UPDATE column
		# list. The legacy per-kind tables, journal, receipts, snapshot staging
		# and wake queue are gone (000030 section 8; the Go schema tests assert
		# their absence).
		if [ "$(sql "SELECT current_version >= 30 FROM schema_compatibility WHERE singleton")" = t ]; then
		  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT') AND has_table_privilege('baby_tracker_runtime',t,'INSERT') AND NOT has_table_privilege('baby_tracker_runtime',t,'UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public.care_entities','public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t")" = t
		  test "$(sql "SELECT NOT bool_or(has_any_column_privilege('baby_tracker_runtime',t,'UPDATE')) FROM unnest(ARRAY['public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t")" = t
		  test "$(sql "SELECT NOT bool_or(has_any_column_privilege('baby_tracker_runtime',t,'REFERENCES')) FROM unnest(ARRAY['public.care_entities','public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t")" = t
		  test "$(sql "SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid='public.care_entities'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege('baby_tracker_runtime',attrelid,attnum,'UPDATE')")" = baby_id,entity_version,changed_seq,actor_account_id,device_id,operation,accepted_at,tombstoned_at,started_at,ended_at,session_id,completed_record_id,sort_order,label,name_hash,breast_seconds,bottle_volume_ml,bottle_observations,bottle_missing,shortest_part_seconds,wire,feed_kind
		  test "$(sql "SELECT bool_and(has_column_privilege('baby_tracker_runtime','public.families',c,'UPDATE')) FROM unnest(ARRAY['care_record_count','care_charged_bytes','care_entity_count','care_wire_bytes']) c")" = t
		  test "$(sql "SELECT NOT bool_or(has_table_privilege(r,t,p)) FROM unnest(ARRAY['public']) r CROSS JOIN unnest(ARRAY['public.care_entities','public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  test "$(sql "SELECT NOT bool_or(has_any_column_privilege(r,t,p)) FROM unnest(ARRAY['public']) r CROSS JOIN unnest(ARRAY['public.care_entities','public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) p")" = t
		fi
		# Schema 31 (Phase 3): the maintenance group runs care retention, the
		# family purge, wakes and audit partition rolling. It deletes care rows
		# and import sessions (each privilege checked on its own) and never
		# inserts, updates or truncates one; it reads whole receipts, bindings
		# and snapshot sessions, but care rows and import sessions through
		# their keys only (exact column lists: no care payload); on families it
		# reads seven named columns and updates exactly the four usage
		# counters; it alone writes device_sync_cursors and executes the five
		# maintenance definers (care rows and import staging it deletes only
		# through app.purge_family_step). The runtime registers its own device's push
		# token (SELECT, INSERT, DELETE, and UPDATE of exactly environment,
		# token, registered_at); the maintenance group holds nothing on the
		# token table (it goes through the definers). The database rate limits
		# are gone.
		if [ "$(sql "SELECT current_version >= 31 FROM schema_compatibility WHERE singleton")" = t ]; then
		  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_maintenance',t,'DELETE') AND NOT has_table_privilege('baby_tracker_maintenance',t,'INSERT') AND NOT has_table_privilege('baby_tracker_maintenance',t,'UPDATE') AND NOT has_table_privilege('baby_tracker_maintenance',t,'TRUNCATE')) FROM unnest(ARRAY['public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t")" = t
		  test "$(sql "SELECT NOT has_table_privilege('baby_tracker_maintenance','public.care_entities','INSERT,UPDATE,DELETE,TRUNCATE') AND NOT has_table_privilege('baby_tracker_maintenance','public.import_sessions','INSERT,UPDATE,DELETE,TRUNCATE')")" = t
		  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_maintenance',t,'SELECT')) FROM unnest(ARRAY['public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t")" = t
		  test "$(sql "SELECT NOT has_table_privilege('baby_tracker_maintenance','public.care_entities','SELECT') AND NOT has_table_privilege('baby_tracker_maintenance','public.import_sessions','SELECT')")" = t
		  test "$(sql "SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid='public.care_entities'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege('baby_tracker_maintenance',attrelid,attnum,'SELECT')")" = family_id,id,kind
		  test "$(sql "SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid='public.import_sessions'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege('baby_tracker_maintenance',attrelid,attnum,'SELECT')")" = family_id,import_id
		  test "$(sql "SELECT NOT bool_or(has_any_column_privilege('baby_tracker_maintenance',t,p)) FROM unnest(ARRAY['public.care_entities','public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions','public.import_sessions']) t CROSS JOIN unnest(ARRAY['INSERT','UPDATE','REFERENCES']) p")" = t
		  test "$(sql "SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid='public.families'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege('baby_tracker_maintenance',attrelid,attnum,'UPDATE')")" = care_record_count,care_charged_bytes,care_entity_count,care_wire_bytes
		  test "$(sql "SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid='public.families'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege('baby_tracker_maintenance',attrelid,attnum,'SELECT')")" = id,status,next_sequence,care_record_count,care_charged_bytes,care_entity_count,care_wire_bytes
		  test "$(sql "SELECT NOT has_table_privilege('baby_tracker_maintenance','public.families','INSERT,UPDATE,DELETE,TRUNCATE')")" = t
		  test "$(sql "SELECT has_table_privilege('baby_tracker_maintenance','public.device_sync_cursors','SELECT') AND has_table_privilege('baby_tracker_maintenance','public.device_sync_cursors','INSERT') AND has_table_privilege('baby_tracker_maintenance','public.device_sync_cursors','UPDATE') AND has_table_privilege('baby_tracker_maintenance','public.device_sync_cursors','DELETE') AND NOT has_table_privilege('baby_tracker_maintenance','public.device_sync_cursors','TRUNCATE')")" = t
		  test "$(sql "SELECT has_table_privilege('baby_tracker_runtime','public.device_push_tokens','SELECT') AND has_table_privilege('baby_tracker_runtime','public.device_push_tokens','INSERT') AND has_table_privilege('baby_tracker_runtime','public.device_push_tokens','DELETE') AND NOT has_table_privilege('baby_tracker_runtime','public.device_push_tokens','UPDATE,TRUNCATE')")" = t
		  test "$(sql "SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid='public.device_push_tokens'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege('baby_tracker_runtime',attrelid,attnum,'UPDATE')")" = environment,token,registered_at
		  test "$(sql "SELECT NOT has_table_privilege('baby_tracker_maintenance','public.device_push_tokens','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') AND NOT has_any_column_privilege('baby_tracker_maintenance','public.device_push_tokens','SELECT,INSERT,UPDATE,REFERENCES')")" = t
		  test "$(sql "SELECT NOT has_table_privilege('baby_tracker_runtime','public.device_sync_cursors','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') AND NOT has_any_column_privilege('baby_tracker_runtime','public.device_sync_cursors','SELECT,INSERT,UPDATE,REFERENCES')")" = t
		  test "$(sql "SELECT NOT bool_or(has_table_privilege(r,t,p)) FROM unnest(ARRAY['public']) r CROSS JOIN unnest(ARRAY['public.device_sync_cursors','public.device_push_tokens']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  test "$(sql "SELECT NOT bool_or(has_any_column_privilege(r,t,p)) FROM unnest(ARRAY['public']) r CROSS JOIN unnest(ARRAY['public.device_sync_cursors','public.device_push_tokens']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) p")" = t
		  test "$(sql "SELECT bool_and(has_function_privilege('baby_tracker_maintenance',f,'EXECUTE') AND NOT has_function_privilege('public',f,'EXECUTE') AND NOT has_function_privilege('baby_tracker_runtime',f,'EXECUTE')) FROM unnest(ARRAY['app.push_targets(uuid[],uuid[])','app.prune_push_tokens(integer)','app.forget_push_token(uuid,bytea)','app.ensure_audit_partitions(timestamp with time zone)','app.purge_family_step(uuid,integer)']) f")" = t
		  test "$(sql "SELECT to_regclass('public.rate_limit_buckets') IS NULL AND to_regprocedure('app.take_rate_limits(jsonb,timestamp with time zone)') IS NULL")" = t
		fi
		# Schema 32 (Phase 4): Live Activity push on the authority schema. The
		# runtime registers its own devices and card tokens: SELECT, INSERT and
		# UPDATE on the two tenant tables (each privilege checked on its own),
		# never DELETE, and it alone executes the five registration definers. The
		# maintenance group (the Live Activity worker) holds nothing on any Live
		# Activity table and alone executes the nine worker definers. The
		# scheduling tables are definer-only: no principal holds a privilege on
		# them, and no principal executes the internal definers and triggers.
		# main's journal trigger and registration enumeration are not ported.
		if [ "$(sql "SELECT current_version >= 32 FROM schema_compatibility WHERE singleton")" = t ]; then
		  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT') AND has_table_privilege('baby_tracker_runtime',t,'INSERT') AND has_table_privilege('baby_tracker_runtime',t,'UPDATE') AND NOT has_table_privilege('baby_tracker_runtime',t,'DELETE') AND NOT has_table_privilege('baby_tracker_runtime',t,'TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public.live_activity_devices','public.live_activity_tokens']) t")" = t
		  test "$(sql "SELECT NOT bool_or(has_table_privilege(r,t,p) OR has_any_column_privilege(r,t,'SELECT,INSERT,UPDATE,REFERENCES')) FROM unnest(ARRAY['public','baby_tracker_maintenance']) r CROSS JOIN unnest(ARRAY['public.live_activity_devices','public.live_activity_tokens']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  test "$(sql "SELECT NOT bool_or(has_table_privilege(r,t,p) OR has_any_column_privilege(r,t,'SELECT,INSERT,UPDATE,REFERENCES')) FROM unnest(ARRAY['public','baby_tracker_runtime','baby_tracker_maintenance']) r CROSS JOIN unnest(ARRAY['public.live_activity_token_owners','public.live_activity_registration_index','public.live_activity_family_dirty','public.live_activity_delivery','public.live_activity_installations','public.live_activity_history']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  test "$(sql "SELECT bool_and(has_function_privilege('baby_tracker_runtime',f,'EXECUTE') AND NOT has_function_privilege('public',f,'EXECUTE') AND NOT has_function_privilege('baby_tracker_maintenance',f,'EXECUTE')) FROM unnest(ARRAY['app.fence_live_activity_installation(uuid,bigint,uuid,uuid,uuid,uuid,bigint,timestamp with time zone,boolean)','app.live_activity_session_binding(uuid,uuid,uuid,bigint)','app.live_activity_claim_live(jsonb)','app.live_activity_registration_state(uuid,jsonb)','app.live_activity_authorized(uuid,bigint,boolean)']) f")" = t
		  test "$(sql "SELECT bool_and(has_function_privilege('baby_tracker_maintenance',f,'EXECUTE') AND NOT has_function_privilege('public',f,'EXECUTE') AND NOT has_function_privilege('baby_tracker_runtime',f,'EXECUTE')) FROM unnest(ARRAY['app.claim_live_activity_work(timestamp with time zone,integer)','app.finish_live_activity_work(jsonb,timestamp with time zone,text)','app.retain_live_activity_work()','app.live_activity_target(jsonb,jsonb,boolean,timestamp with time zone,jsonb,jsonb)','app.abandon_live_activity_start(jsonb,text,text,boolean,boolean)','app.release_stale_live_activity_lease(jsonb)','app.dirty_live_activity_families(uuid[],bigint[])','app.sweep_live_activity_work()','app.sweep_live_activity_registrations(boolean,integer)']) f")" = t
		  test "$(sql "SELECT NOT bool_or(has_function_privilege(r,f,'EXECUTE')) FROM unnest(ARRAY['public','baby_tracker_runtime','baby_tracker_maintenance']) r CROSS JOIN unnest(ARRAY['app.live_activity_lineage_live(uuid,uuid,uuid,bigint,timestamp with time zone)','app.dirty_live_activity_family(uuid,bigint)','app.retire_stale_live_activity_registrations()','app.live_activity_token_ownership()','app.live_activity_registration_dirty()','app.live_activity_token_fingerprint()']) f")" = t
		  test "$(sql "SELECT to_regprocedure('app.live_activity_journal_dirty()') IS NULL AND to_regprocedure('app.live_activity_registration_subjects(uuid,integer)') IS NULL")" = t
		fi
    test "$(sql "SELECT has_schema_privilege('baby_tracker_runtime', 'app', 'USAGE') AND has_schema_privilege('baby_tracker_runtime', 'public', 'USAGE') AND has_function_privilege('baby_tracker_runtime', 'app.current_account_id()', 'EXECUTE') AND has_function_privilege('baby_tracker_runtime', 'app.current_family_id()', 'EXECUTE')")" = t
  fi
  echo 'principal proof: positive role and exact current privilege contract verified; destructive probes delegated to real-PostgreSQL tests'
  exit 0
fi
for principal in baby_tracker_migrator baby_tracker_app baby_tracker_export_app; do
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
if [ "$(sql "SELECT to_regclass('public.schema_migrations') IS NOT NULL")" = t ]; then
  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime', t, 'SELECT')) FROM unnest(ARRAY['public.schema_migrations','public.server_state','public.schema_compatibility','public.schema_privilege_manifest']) t")" = t
  if [ "$(sql "SELECT current_version >= 6 FROM schema_compatibility WHERE singleton")" = t ]; then
    test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime', t, 'SELECT') AND NOT has_table_privilege('baby_tracker_runtime', t, 'INSERT,UPDATE,DELETE,TRUNCATE')) FROM unnest(ARRAY['public.accounts','public.families','public.memberships']) t")" = t
  else
    test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime', t, 'SELECT') AND has_table_privilege('baby_tracker_runtime', t, 'INSERT') AND has_table_privilege('baby_tracker_runtime', t, 'UPDATE') AND has_table_privilege('baby_tracker_runtime', t, 'DELETE')) FROM unnest(ARRAY['public.accounts','public.families','public.memberships']) t")" = t
  fi
  test "$(sql "SELECT has_schema_privilege('baby_tracker_runtime', 'app', 'USAGE') AND has_schema_privilege('baby_tracker_runtime', 'public', 'USAGE') AND has_function_privilege('baby_tracker_runtime', 'app.current_account_id()', 'EXECUTE') AND has_function_privilege('baby_tracker_runtime', 'app.current_family_id()', 'EXECUTE')")" = t

  reject_catalog_state 'maintenance group tenant UPDATE on families' \
    'GRANT UPDATE ON public.families TO baby_tracker_maintenance' \
    "SELECT has_table_privilege('baby_tracker_export_app', 'public.families', 'UPDATE')" \
    'REVOKE UPDATE ON public.families FROM baby_tracker_maintenance'
  reject_catalog_state 'maintenance group tenant DELETE on memberships' \
    'GRANT DELETE ON public.memberships TO baby_tracker_maintenance' \
    "SELECT has_table_privilege('baby_tracker_export_app', 'public.memberships', 'DELETE')" \
    'REVOKE DELETE ON public.memberships FROM baby_tracker_maintenance'
  reject_catalog_state 'maintenance group tenant context helper' \
    'GRANT EXECUTE ON FUNCTION app.current_family_id() TO baby_tracker_maintenance' \
    "SELECT has_function_privilege('baby_tracker_export_app', 'app.current_family_id()', 'EXECUTE')" \
    'REVOKE EXECUTE ON FUNCTION app.current_family_id() FROM baby_tracker_maintenance'

	if [ "$(sql "SELECT current_version >= 9 FROM schema_compatibility WHERE singleton")" = t ]; then
	  test "$(sql "SELECT has_column_privilege('baby_tracker_runtime','public.families','next_sequence','UPDATE') AND NOT has_table_privilege('baby_tracker_runtime','public.families','UPDATE') AND NOT has_column_privilege('baby_tracker_runtime','public.families','status','UPDATE') AND NOT has_column_privilege('baby_tracker_runtime','public.families','display_name','UPDATE') AND NOT has_column_privilege('baby_tracker_runtime','public.families','purge_after','UPDATE')")" = t
	fi
		if [ "$(sql "SELECT current_version >= 12 FROM schema_compatibility WHERE singleton")" = t ]; then
		  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT,UPDATE') AND NOT has_table_privilege('baby_tracker_runtime',t,'DELETE,TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public.import_sessions','public.import_conflicts']) t")" = t
		  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT') AND NOT has_table_privilege('baby_tracker_runtime',t,'UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public.import_chunks','public.import_baby_candidates','public.import_record_manifests','public.import_conflict_receipts','public.import_baby_mappings']) t")" = t
		  test "$(sql "SELECT NOT bool_or(has_table_privilege(r,t,p)) FROM unnest(ARRAY['public','baby_tracker_maintenance']) r CROSS JOIN unnest(ARRAY['public.import_sessions','public.import_chunks','public.import_baby_candidates','public.import_record_manifests','public.import_conflicts','public.import_conflict_receipts','public.import_baby_mappings']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  test "$(sql "SELECT has_function_privilege('baby_tracker_runtime','app.import_unfinished_session_count(uuid,timestamp with time zone)','EXECUTE') AND NOT has_function_privilege('public','app.import_unfinished_session_count(uuid,timestamp with time zone)','EXECUTE') AND NOT has_function_privilege('baby_tracker_maintenance','app.import_unfinished_session_count(uuid,timestamp with time zone)','EXECUTE')")" = t
		fi
		# Schema 30 (Phase 2): care data lives in care_entities, beside its
		# receipts, continuation bindings and snapshot sessions. The runtime reads
		# and inserts them and updates only care_entities' named columns and the
		# family usage counters; PUBLIC never reaches them (the maintenance
		# group's retention grants are schema 31's, below). has_table_privilege sees only table-level
		# grants, so column-level grants are checked with
		# has_any_column_privilege and the exact care_entities UPDATE column
		# list. The legacy per-kind tables, journal, receipts, snapshot staging
		# and wake queue are gone (000030 section 8; the Go schema tests assert
		# their absence).
		if [ "$(sql "SELECT current_version >= 30 FROM schema_compatibility WHERE singleton")" = t ]; then
		  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT') AND has_table_privilege('baby_tracker_runtime',t,'INSERT') AND NOT has_table_privilege('baby_tracker_runtime',t,'UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public.care_entities','public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t")" = t
		  test "$(sql "SELECT NOT bool_or(has_any_column_privilege('baby_tracker_runtime',t,'UPDATE')) FROM unnest(ARRAY['public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t")" = t
		  test "$(sql "SELECT NOT bool_or(has_any_column_privilege('baby_tracker_runtime',t,'REFERENCES')) FROM unnest(ARRAY['public.care_entities','public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t")" = t
		  test "$(sql "SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid='public.care_entities'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege('baby_tracker_runtime',attrelid,attnum,'UPDATE')")" = baby_id,entity_version,changed_seq,actor_account_id,device_id,operation,accepted_at,tombstoned_at,started_at,ended_at,session_id,completed_record_id,sort_order,label,name_hash,breast_seconds,bottle_volume_ml,bottle_observations,bottle_missing,shortest_part_seconds,wire,feed_kind
		  test "$(sql "SELECT bool_and(has_column_privilege('baby_tracker_runtime','public.families',c,'UPDATE')) FROM unnest(ARRAY['care_record_count','care_charged_bytes','care_entity_count','care_wire_bytes']) c")" = t
		  test "$(sql "SELECT NOT bool_or(has_table_privilege(r,t,p)) FROM unnest(ARRAY['public']) r CROSS JOIN unnest(ARRAY['public.care_entities','public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  test "$(sql "SELECT NOT bool_or(has_any_column_privilege(r,t,p)) FROM unnest(ARRAY['public']) r CROSS JOIN unnest(ARRAY['public.care_entities','public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) p")" = t
		fi
		# Schema 31 (Phase 3): the maintenance group runs care retention, the
		# family purge, wakes and audit partition rolling. It deletes care rows
		# and import sessions (each privilege checked on its own) and never
		# inserts, updates or truncates one; it reads whole receipts, bindings
		# and snapshot sessions, but care rows and import sessions through
		# their keys only (exact column lists: no care payload); on families it
		# reads seven named columns and updates exactly the four usage
		# counters; it alone writes device_sync_cursors and executes the five
		# maintenance definers (care rows and import staging it deletes only
		# through app.purge_family_step). The runtime registers its own device's push
		# token (SELECT, INSERT, DELETE, and UPDATE of exactly environment,
		# token, registered_at); the maintenance group holds nothing on the
		# token table (it goes through the definers). The database rate limits
		# are gone.
		if [ "$(sql "SELECT current_version >= 31 FROM schema_compatibility WHERE singleton")" = t ]; then
		  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_maintenance',t,'DELETE') AND NOT has_table_privilege('baby_tracker_maintenance',t,'INSERT') AND NOT has_table_privilege('baby_tracker_maintenance',t,'UPDATE') AND NOT has_table_privilege('baby_tracker_maintenance',t,'TRUNCATE')) FROM unnest(ARRAY['public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t")" = t
		  test "$(sql "SELECT NOT has_table_privilege('baby_tracker_maintenance','public.care_entities','INSERT,UPDATE,DELETE,TRUNCATE') AND NOT has_table_privilege('baby_tracker_maintenance','public.import_sessions','INSERT,UPDATE,DELETE,TRUNCATE')")" = t
		  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_maintenance',t,'SELECT')) FROM unnest(ARRAY['public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions']) t")" = t
		  test "$(sql "SELECT NOT has_table_privilege('baby_tracker_maintenance','public.care_entities','SELECT') AND NOT has_table_privilege('baby_tracker_maintenance','public.import_sessions','SELECT')")" = t
		  test "$(sql "SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid='public.care_entities'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege('baby_tracker_maintenance',attrelid,attnum,'SELECT')")" = family_id,id,kind
		  test "$(sql "SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid='public.import_sessions'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege('baby_tracker_maintenance',attrelid,attnum,'SELECT')")" = family_id,import_id
		  test "$(sql "SELECT NOT bool_or(has_any_column_privilege('baby_tracker_maintenance',t,p)) FROM unnest(ARRAY['public.care_entities','public.care_receipts','public.sync_page_bindings','public.sync_snapshot_sessions','public.import_sessions']) t CROSS JOIN unnest(ARRAY['INSERT','UPDATE','REFERENCES']) p")" = t
		  test "$(sql "SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid='public.families'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege('baby_tracker_maintenance',attrelid,attnum,'UPDATE')")" = care_record_count,care_charged_bytes,care_entity_count,care_wire_bytes
		  test "$(sql "SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid='public.families'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege('baby_tracker_maintenance',attrelid,attnum,'SELECT')")" = id,status,next_sequence,care_record_count,care_charged_bytes,care_entity_count,care_wire_bytes
		  test "$(sql "SELECT NOT has_table_privilege('baby_tracker_maintenance','public.families','INSERT,UPDATE,DELETE,TRUNCATE')")" = t
		  test "$(sql "SELECT has_table_privilege('baby_tracker_maintenance','public.device_sync_cursors','SELECT') AND has_table_privilege('baby_tracker_maintenance','public.device_sync_cursors','INSERT') AND has_table_privilege('baby_tracker_maintenance','public.device_sync_cursors','UPDATE') AND has_table_privilege('baby_tracker_maintenance','public.device_sync_cursors','DELETE') AND NOT has_table_privilege('baby_tracker_maintenance','public.device_sync_cursors','TRUNCATE')")" = t
		  test "$(sql "SELECT has_table_privilege('baby_tracker_runtime','public.device_push_tokens','SELECT') AND has_table_privilege('baby_tracker_runtime','public.device_push_tokens','INSERT') AND has_table_privilege('baby_tracker_runtime','public.device_push_tokens','DELETE') AND NOT has_table_privilege('baby_tracker_runtime','public.device_push_tokens','UPDATE,TRUNCATE')")" = t
		  test "$(sql "SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid='public.device_push_tokens'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege('baby_tracker_runtime',attrelid,attnum,'UPDATE')")" = environment,token,registered_at
		  test "$(sql "SELECT NOT has_table_privilege('baby_tracker_maintenance','public.device_push_tokens','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') AND NOT has_any_column_privilege('baby_tracker_maintenance','public.device_push_tokens','SELECT,INSERT,UPDATE,REFERENCES')")" = t
		  test "$(sql "SELECT NOT has_table_privilege('baby_tracker_runtime','public.device_sync_cursors','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') AND NOT has_any_column_privilege('baby_tracker_runtime','public.device_sync_cursors','SELECT,INSERT,UPDATE,REFERENCES')")" = t
		  test "$(sql "SELECT NOT bool_or(has_table_privilege(r,t,p)) FROM unnest(ARRAY['public']) r CROSS JOIN unnest(ARRAY['public.device_sync_cursors','public.device_push_tokens']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  test "$(sql "SELECT NOT bool_or(has_any_column_privilege(r,t,p)) FROM unnest(ARRAY['public']) r CROSS JOIN unnest(ARRAY['public.device_sync_cursors','public.device_push_tokens']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','REFERENCES']) p")" = t
		  test "$(sql "SELECT bool_and(has_function_privilege('baby_tracker_maintenance',f,'EXECUTE') AND NOT has_function_privilege('public',f,'EXECUTE') AND NOT has_function_privilege('baby_tracker_runtime',f,'EXECUTE')) FROM unnest(ARRAY['app.push_targets(uuid[],uuid[])','app.prune_push_tokens(integer)','app.forget_push_token(uuid,bytea)','app.ensure_audit_partitions(timestamp with time zone)','app.purge_family_step(uuid,integer)']) f")" = t
		  test "$(sql "SELECT to_regclass('public.rate_limit_buckets') IS NULL AND to_regprocedure('app.take_rate_limits(jsonb,timestamp with time zone)') IS NULL")" = t
		fi
		# Schema 32 (Phase 4): Live Activity push on the authority schema. The
		# runtime registers its own devices and card tokens: SELECT, INSERT and
		# UPDATE on the two tenant tables (each privilege checked on its own),
		# never DELETE, and it alone executes the five registration definers. The
		# maintenance group (the Live Activity worker) holds nothing on any Live
		# Activity table and alone executes the nine worker definers. The
		# scheduling tables are definer-only: no principal holds a privilege on
		# them, and no principal executes the internal definers and triggers.
		# main's journal trigger and registration enumeration are not ported.
		if [ "$(sql "SELECT current_version >= 32 FROM schema_compatibility WHERE singleton")" = t ]; then
		  test "$(sql "SELECT bool_and(has_table_privilege('baby_tracker_runtime',t,'SELECT') AND has_table_privilege('baby_tracker_runtime',t,'INSERT') AND has_table_privilege('baby_tracker_runtime',t,'UPDATE') AND NOT has_table_privilege('baby_tracker_runtime',t,'DELETE') AND NOT has_table_privilege('baby_tracker_runtime',t,'TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public.live_activity_devices','public.live_activity_tokens']) t")" = t
		  test "$(sql "SELECT NOT bool_or(has_table_privilege(r,t,p) OR has_any_column_privilege(r,t,'SELECT,INSERT,UPDATE,REFERENCES')) FROM unnest(ARRAY['public','baby_tracker_maintenance']) r CROSS JOIN unnest(ARRAY['public.live_activity_devices','public.live_activity_tokens']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  test "$(sql "SELECT NOT bool_or(has_table_privilege(r,t,p) OR has_any_column_privilege(r,t,'SELECT,INSERT,UPDATE,REFERENCES')) FROM unnest(ARRAY['public','baby_tracker_runtime','baby_tracker_maintenance']) r CROSS JOIN unnest(ARRAY['public.live_activity_token_owners','public.live_activity_registration_index','public.live_activity_family_dirty','public.live_activity_delivery','public.live_activity_installations','public.live_activity_history']) t CROSS JOIN unnest(ARRAY['SELECT','INSERT','UPDATE','DELETE','TRUNCATE','REFERENCES','TRIGGER']) p")" = t
		  test "$(sql "SELECT bool_and(has_function_privilege('baby_tracker_runtime',f,'EXECUTE') AND NOT has_function_privilege('public',f,'EXECUTE') AND NOT has_function_privilege('baby_tracker_maintenance',f,'EXECUTE')) FROM unnest(ARRAY['app.fence_live_activity_installation(uuid,bigint,uuid,uuid,uuid,uuid,bigint,timestamp with time zone,boolean)','app.live_activity_session_binding(uuid,uuid,uuid,bigint)','app.live_activity_claim_live(jsonb)','app.live_activity_registration_state(uuid,jsonb)','app.live_activity_authorized(uuid,bigint,boolean)']) f")" = t
		  test "$(sql "SELECT bool_and(has_function_privilege('baby_tracker_maintenance',f,'EXECUTE') AND NOT has_function_privilege('public',f,'EXECUTE') AND NOT has_function_privilege('baby_tracker_runtime',f,'EXECUTE')) FROM unnest(ARRAY['app.claim_live_activity_work(timestamp with time zone,integer)','app.finish_live_activity_work(jsonb,timestamp with time zone,text)','app.retain_live_activity_work()','app.live_activity_target(jsonb,jsonb,boolean,timestamp with time zone,jsonb,jsonb)','app.abandon_live_activity_start(jsonb,text,text,boolean,boolean)','app.release_stale_live_activity_lease(jsonb)','app.dirty_live_activity_families(uuid[],bigint[])','app.sweep_live_activity_work()','app.sweep_live_activity_registrations(boolean,integer)']) f")" = t
		  test "$(sql "SELECT NOT bool_or(has_function_privilege(r,f,'EXECUTE')) FROM unnest(ARRAY['public','baby_tracker_runtime','baby_tracker_maintenance']) r CROSS JOIN unnest(ARRAY['app.live_activity_lineage_live(uuid,uuid,uuid,bigint,timestamp with time zone)','app.dirty_live_activity_family(uuid,bigint)','app.retire_stale_live_activity_registrations()','app.live_activity_token_ownership()','app.live_activity_registration_dirty()','app.live_activity_token_fingerprint()']) f")" = t
		  test "$(sql "SELECT to_regprocedure('app.live_activity_journal_dirty()') IS NULL AND to_regprocedure('app.live_activity_registration_subjects(uuid,integer)') IS NULL")" = t
		fi
fi
sql 'DROP TABLE bootstrap_probe.acl_probe; DROP SCHEMA bootstrap_probe' >/dev/null
bootstrap >/dev/null
if [ "$failures" -ne 0 ]; then
  echo "catalog proof FAILED: $failures incompatible states accepted" >&2
  exit 1
fi
echo 'catalog proof: login/group ACL and ownership attacks rejected; compatible tenant DML, operational SELECT, schema USAGE, function EXECUTE and incoming membership preserved'
