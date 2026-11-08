#!/usr/bin/env bash
set -euo pipefail

wait_for_ready_status() {
  local expected=$1
  local observed=""
  for _ in $(seq 1 100); do
    if ! observed=$(request /readyz 0.2); then
      observed="unreachable"
    fi
    if [[ $observed == "$expected" ]]; then
      return 0
    fi
    sleep 0.1
  done
  echo "readiness remained $observed, wanted $expected" >&2
  return 1
}

# recreate_app restarts the app container and points base at its new port.
recreate_app() {
  compose up -d --force-recreate app >/dev/null
  binding=$(compose port app 8080)
  [[ $binding == 127.0.0.1:* ]]
  base="http://$binding"
}

smoke_rejects_actor_id_pepper_replacement() {
  local actor_file="$BT_SECRETS_DIR/actor_id_pepper"
  local original_file="$BT_SECRETS_DIR/actor_id_pepper.original"
  local verification=""
  cp "$actor_file" "$original_file"
  compose up -d --force-recreate app >/dev/null
  binding=$(compose port app 8080)
  [[ $binding == 127.0.0.1:* ]]
  base="http://$binding"
  wait_for_ready_status 200
  chmod 0600 "$actor_file"
  while :; do
    openssl rand -hex 32 > "$actor_file"
    cmp -s "$actor_file" "$original_file" || break
  done
  chmod 0444 "$actor_file"
  compose up -d --force-recreate app >/dev/null
  binding=$(compose port app 8080)
  [[ $binding == 127.0.0.1:* ]]
  base="http://$binding"
  # server_state commits to the original actor-ID key: the node refuses
  # (readiness 503) and the authority verification names the mismatch.
  wait_for_ready_status 503
  if verification=$(compose run --rm --no-deps migrate authority verify 2>&1); then
    echo "replacement actor ID pepper unexpectedly verified" >&2
    return 1
  fi
  [[ $verification == *actor_id_key_mismatch* ]]
  chmod 0600 "$actor_file"
  cp "$original_file" "$actor_file"
  chmod 0444 "$actor_file"
  compose up -d --force-recreate app >/dev/null
  binding=$(compose port app 8080)
  [[ $binding == 127.0.0.1:* ]]
  base="http://$binding"
  wait_for_ready_status 200
}

if [[ ${BT_SMOKE_LIBRARY_ONLY:-0} == 1 ]]; then
  return 0 2>/dev/null || exit 0
fi

cd "$(dirname "$0")/../.."
for tool in docker curl jq openssl; do command -v "$tool" >/dev/null; done
project="baby-tracker-smoke-$(date +%s)-$$"
export BT_SECRETS_DIR
BT_SECRETS_DIR=$(mktemp -d "${TMPDIR:-/tmp}/baby-tracker-smoke.XXXXXXXX")
export BT_IMAGE_NAME="$project:local"
export BT_SERVER_INSTANCE_ID=123e4567-e89b-12d3-a456-426614174000
export BT_RECOVERY_EPOCH=1 BT_APPLE_AUDIENCE=com.example.babytracker.smoke
export BT_MODE=self_hosted BT_PUBLIC_ORIGIN=https://sync.example.test
export BT_IDENTITY_KEY_ID=identity.v1 BT_SESSION_TOKEN_KEY_ID=session.v1
export BT_DEVICE_CONTINUITY_KEY_ID=device.v1 BT_APPLE_NOTIFICATION_KEY_ID=notification.v1
export BT_INVITATION_KEY_ID=invitation.v1
export BT_PAGE_TOKEN_KEY_ID=page.v1
export BT_HTTP_PORT=0 BT_TRUSTED_PROXY_CIDRS=127.0.0.1/32
compose() { docker compose --project-name "$project" -f Server/compose.yaml "$@"; }
smoke_failure() {
  local line=$1
  echo "smoke assertion failed at line $line" >&2
  compose logs --no-color --tail 200 db >&2 || true
}
cleanup() {
  status=$?
  trap - EXIT
  set +e
  cleanup_ok=1
  compose down --volumes --remove-orphans >/dev/null 2>&1 || cleanup_ok=0
  remaining_containers=$(docker ps -aq --filter "label=com.docker.compose.project=$project") || cleanup_ok=0
  remaining_volumes=$(docker volume ls -q --filter "label=com.docker.compose.project=$project") || cleanup_ok=0
  if [[ $cleanup_ok -eq 0 || -n $remaining_containers || -n $remaining_volumes ]]; then
    echo "cleanup FAILED for project $project" >&2
    [[ $status -ne 0 ]] || status=1
  else
    echo "cleanup proof: $project has zero containers and zero volumes"
  fi
  image_cleanup_ok=1
  temporary_image=$(docker image ls --quiet --filter "reference=$BT_IMAGE_NAME") || image_cleanup_ok=0
  if [[ -n $temporary_image ]]; then
    docker image rm "$BT_IMAGE_NAME" >/dev/null 2>&1 || image_cleanup_ok=0
  fi
  remaining_image=$(docker image ls --quiet --filter "reference=$BT_IMAGE_NAME") || image_cleanup_ok=0
  if [[ $image_cleanup_ok -eq 0 || -n $remaining_image ]]; then
    echo "cleanup FAILED for temporary image $BT_IMAGE_NAME" >&2
    [[ $status -ne 0 ]] || status=1
  else
    echo "cleanup proof: temporary image $BT_IMAGE_NAME is absent"
  fi
  rm -f "$BT_SECRETS_DIR/bootstrap_password" "$BT_SECRETS_DIR/migrator_password" \
    "$BT_SECRETS_DIR/runtime_password" "$BT_SECRETS_DIR/export_password" "$BT_SECRETS_DIR/migrator_database_url" \
    "$BT_SECRETS_DIR/runtime_database_url" "$BT_SECRETS_DIR/maintenance_database_url" \
	"$BT_SECRETS_DIR/identity_pepper" "$BT_SECRETS_DIR/session_token_pepper" \
	"$BT_SECRETS_DIR/device_continuity_pepper" "$BT_SECRETS_DIR/apple_notification_pepper" \
	"$BT_SECRETS_DIR/invitation_pepper" \
	"$BT_SECRETS_DIR/page_token_key" \
	"$BT_SECRETS_DIR/actor_id_pepper" "$BT_SECRETS_DIR/actor_id_pepper.original" "$BT_SECRETS_DIR/rate_limit_pepper" \
    "$BT_SECRETS_DIR/response.json"
  rmdir "$BT_SECRETS_DIR" || { [[ $status -ne 0 ]] || status=1; }
  if [[ ! -e $BT_SECRETS_DIR ]]; then
    echo "cleanup proof: temporary secret directory removed"
  else
    [[ $status -ne 0 ]] || status=1
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'smoke_failure "$LINENO"' ERR
trap 'exit 130' INT
trap 'exit 143' TERM
umask 077
for secret in bootstrap_password migrator_password runtime_password export_password identity_pepper session_token_pepper device_continuity_pepper apple_notification_pepper invitation_pepper page_token_key actor_id_pepper rate_limit_pepper; do
  openssl rand -hex 32 > "$BT_SECRETS_DIR/$secret"
done
printf 'postgres://baby_tracker_migrator:%s@db:5432/babytracker?sslmode=disable\n' \
  "$(<"$BT_SECRETS_DIR/migrator_password")" > "$BT_SECRETS_DIR/migrator_database_url"
printf 'postgres://baby_tracker_app:%s@db:5432/babytracker?sslmode=disable\n' \
  "$(<"$BT_SECRETS_DIR/runtime_password")" > "$BT_SECRETS_DIR/runtime_database_url"
# The maintenance credential is required in every mode (decision D3).
printf 'postgres://baby_tracker_export_app:%s@db:5432/babytracker?sslmode=disable\n' \
  "$(<"$BT_SECRETS_DIR/export_password")" > "$BT_SECRETS_DIR/maintenance_database_url"
# Local Compose bind-mounted secrets keep host ownership. The private directory
# is 0700; files must be readable by the container's fixed nonroot UID.
chmod 0444 "$BT_SECRETS_DIR"/*
compose config --quiet
compose up -d --build --wait --wait-timeout 120 db app
app=$(compose ps -q app)
db=$(compose ps -q db)
[[ $(docker inspect --format '{{.Config.User}}' "$app") == 65532:65532 ]]
[[ $(docker inspect --format '{{.State.Health.Status}}' "$app") == healthy ]]
[[ $(docker inspect --format '{{.HostConfig.ReadonlyRootfs}}' "$app") == true ]]
docker inspect "$app" | jq -e '.[0].Config.Healthcheck.Test | index("/usr/local/bin/baby-tracker-server") != null' >/dev/null
# PostgreSQL is the only security authority: the app has no state volume.
docker inspect "$app" | jq -e '.[0].Mounts | map(select(.Type == "volume")) | length == 0' >/dev/null
docker export "$app" | tar -tf - | awk '
  /(^|\/)(sh|bash|ash|dash|busybox|apk|apt|apt-get|dpkg|yum|dnf)$/ { bad=1 }
  END { exit bad }'
echo "image proof: $(docker inspect --format '{{.Image}}' "$app"); UID/GID 65532; read-only; own-binary healthcheck; no shell/package manager"
binding=$(compose port app 8080)
[[ $binding == 127.0.0.1:* ]]
base="http://$binding"
request() {
  local path=$1
  local timeout=${2:-5}
  curl --silent --show-error --connect-timeout "$timeout" --max-time "$timeout" -o "$BT_SECRETS_DIR/response.json" -w '%{http_code}' "$base$path"
}
[[ $(request /healthz) == 200 ]]
[[ $(request /readyz) == 503 ]]
jq -e '.error.code == "not_ready" and .error.retryable == true' "$BT_SECRETS_DIR/response.json" >/dev/null
echo 'pre-migration proof: health 200, readiness 503'
docker exec -e BT_PRINCIPAL_NEGATIVE_PROBES=0 -i "$db" sh -s < Server/scripts/check-principals.sh
compose run --rm --no-deps migrate
compose run --rm --no-deps migrate authority init --instance-id "$BT_SERVER_INSTANCE_ID" --epoch 1 --confirm-instance "$BT_SERVER_INSTANCE_ID"
echo "migration completed; checking readiness"
ready=0
for _ in $(seq 1 100); do
  if [[ $(request /readyz) == 200 ]]; then ready=1; break; fi
  sleep 0.1
done
[[ $ready == 1 ]]
[[ $(request /v1/server) == 200 ]]
jq . "$BT_SECRETS_DIR/response.json"
# The maintenance credential is required (decision D3), so a ready node
# always advertises refresh-recovery-v1 beyond the fixture's minimal shape.
jq -e --slurpfile expected Server/api/fixtures/server-info.json '
  .instance_id == "123e4567-e89b-12d3-a456-426614174000" and .recovery_epoch == 1 and
  (.capabilities | index("refresh-recovery-v1") != null) and
  ((del(.instance_id,.recovery_epoch) | .capabilities -= ["refresh-recovery-v1"]) == ($expected[0] | del(.instance_id,.recovery_epoch)))
' "$BT_SECRETS_DIR/response.json" >/dev/null
challenge_status=$(curl --silent --show-error --max-time 5 -o "$BT_SECRETS_DIR/response.json" -w '%{http_code}' \
  -H 'Content-Type: application/json' -d '{"protocol_version":"v1","capabilities":["identity-v1"]}' "$base/v1/auth/challenge")
[[ $challenge_status == 200 ]]
jq -e '.protocol_version == "v1" and (.challenge | length) == 43 and .server_instance_id == "123e4567-e89b-12d3-a456-426614174000" and .canonical_origin == "https://sync.example.test"' "$BT_SECRETS_DIR/response.json" >/dev/null
# Push is off here (no BT_APNS_*), so /v1/server above advertises no
# push-token-v1 and the token route is not registered: an unauthenticated PUT
# is 404, where a node with push on would answer 401.
[[ $(curl --silent --max-time 5 -o /dev/null -w '%{http_code}' -X PUT "$base/v1/devices/push-token") == 404 ]]
echo 'push-off proof: no push-token-v1 advertised; PUT /v1/devices/push-token is 404 (route not registered)'
smoke_rejects_actor_id_pepper_replacement
echo 'actor-ID proof: identical restart stays ready; replacement fails authority verify with actor_id_key_mismatch and readiness 503; restored key recovers readiness'
[[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tableowner <> 'baby_tracker_migrator'") == 0 ]]
[[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*) > 0 FROM pg_tables WHERE schemaname='public' AND tableowner = 'baby_tracker_migrator'") == t ]]
expected_schema_version=$(jq -r '.max' Server/internal/migrate/compatibility.json)
[[ $expected_schema_version =~ ^[1-9][0-9]*$ ]]
[[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c 'SELECT current_version FROM schema_compatibility WHERE singleton') == "$expected_schema_version" ]]
# The applied history is the squashed baseline (000032_baseline.sql stands for
# 000001-000032) and every migration after it, contiguous.
baseline_version=$(jq -r '.known[0].version' Server/internal/migrate/compatibility.json)
baseline_name=$(jq -r '.known[0].name' Server/internal/migrate/compatibility.json)
[[ $baseline_version =~ ^[1-9][0-9]*$ && $baseline_name =~ ^[0-9]{6}_baseline\.sql$ ]]
[[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT count(*)=current_version-$baseline_version+1 AND min(version)=$baseline_version AND max(version)=current_version AND bool_or(version=$baseline_version AND name='$baseline_name') FROM schema_migrations CROSS JOIN schema_compatibility WHERE singleton GROUP BY current_version") == t ]]
[[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT has_table_privilege(current_user, 'schema_migrations', 'INSERT,UPDATE,DELETE,TRUNCATE')") == f ]]
# The authority inserts families and updates named columns; nothing else.
[[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT has_table_privilege(current_user, 'families', 'DELETE,TRUNCATE') OR has_table_privilege(current_user, 'families', 'UPDATE')") == f ]]
if (( expected_schema_version >= 9 )); then
  [[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT has_column_privilege(current_user,'families','next_sequence','UPDATE') AND NOT has_table_privilege(current_user,'families','UPDATE') AND NOT has_column_privilege(current_user,'families','id','UPDATE') AND NOT has_column_privilege(current_user,'families','created_at','UPDATE')") == t ]]
fi
if (( expected_schema_version >= 30 )); then
  # Care data lives in care_entities beside its receipts, continuation
  # bindings and snapshot sessions: the app reads and inserts them and updates
  # only care_entities' named columns (exactly the list below) and the family
  # usage counters. has_table_privilege sees only table-level grants, so a
  # column-level UPDATE is checked with has_any_column_privilege. PUBLIC never
  # reaches them (the maintenance group's retention grants are schema 31's,
  # below).
  [[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT bool_and(has_table_privilege(current_user,t,'SELECT') AND has_table_privilege(current_user,t,'INSERT') AND NOT has_table_privilege(current_user,t,'UPDATE,DELETE,TRUNCATE')) FROM unnest(ARRAY['care_entities','care_receipts','sync_page_bindings','sync_snapshot_sessions']) t") == t ]]
  [[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT NOT bool_or(has_any_column_privilege(current_user,t,'UPDATE')) FROM unnest(ARRAY['care_receipts','sync_page_bindings','sync_snapshot_sessions']) t") == t ]]
  [[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid='care_entities'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege(current_user,attrelid,attnum,'UPDATE')") == baby_id,entity_version,changed_seq,actor_account_id,device_id,operation,accepted_at,tombstoned_at,started_at,ended_at,session_id,completed_record_id,sort_order,label,name_hash,breast_seconds,bottle_volume_ml,bottle_observations,bottle_missing,shortest_part_seconds,wire,feed_kind ]]
  [[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT has_column_privilege(current_user,'families','care_charged_bytes','UPDATE')") == t ]]
  [[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT NOT bool_or(has_table_privilege(r,t,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR has_any_column_privilege(r,t,'SELECT,INSERT,UPDATE,REFERENCES')) FROM unnest(ARRAY['public']) r CROSS JOIN unnest(ARRAY['care_entities','care_receipts','sync_page_bindings','sync_snapshot_sessions']) t") == t ]]
  # The legacy wake queue and its scheduler are gone with the legacy care
  # storage (000030 section 8): the write transaction schedules no wake.
  [[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT to_regclass('public.delivery_pending') IS NULL AND NOT EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='app' AND p.proname='schedule_family_wakes')") == t ]]
fi
if (( expected_schema_version >= 31 )); then
  # The maintenance group (care retention, the family purge, wakes, audit
  # partitions) deletes care rows and import sessions, each privilege checked
  # on its own, never inserts, updates or truncates one, reads care rows and
  # import sessions through their keys only (no care payload), and updates
  # exactly the four family usage counters. The runtime writes its own
  # device's push token; the maintenance group holds nothing on the token
  # table (it goes through definers). Rate limits live in process memory:
  # their table is gone.
  [[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT bool_and(has_table_privilege('baby_tracker_maintenance',t,'DELETE') AND NOT has_table_privilege('baby_tracker_maintenance',t,'INSERT,UPDATE,TRUNCATE') AND NOT has_any_column_privilege('baby_tracker_maintenance',t,'INSERT,UPDATE')) FROM unnest(ARRAY['care_receipts','sync_page_bindings','sync_snapshot_sessions']) t") == t ]]
  [[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT NOT bool_or(has_table_privilege('baby_tracker_maintenance',t,'INSERT,UPDATE,DELETE,TRUNCATE') OR has_any_column_privilege('baby_tracker_maintenance',t,'INSERT,UPDATE')) FROM unnest(ARRAY['care_entities','import_sessions']) t") == t ]]
  [[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid='care_entities'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege('baby_tracker_maintenance',attrelid,attnum,'SELECT')") == family_id,id,kind ]]
  [[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute WHERE attrelid='families'::regclass AND attnum > 0 AND NOT attisdropped AND has_column_privilege('baby_tracker_maintenance',attrelid,attnum,'UPDATE')") == care_record_count,care_charged_bytes,care_entity_count,care_wire_bytes ]]
  [[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT has_table_privilege('baby_tracker_maintenance','device_sync_cursors','SELECT') AND has_table_privilege('baby_tracker_maintenance','device_sync_cursors','INSERT') AND has_table_privilege('baby_tracker_maintenance','device_sync_cursors','UPDATE') AND has_table_privilege('baby_tracker_maintenance','device_sync_cursors','DELETE') AND NOT has_table_privilege('baby_tracker_runtime','device_sync_cursors','SELECT,INSERT,UPDATE,DELETE,TRUNCATE')") == t ]]
  [[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT has_table_privilege(current_user,'device_push_tokens','SELECT') AND has_table_privilege(current_user,'device_push_tokens','INSERT') AND has_table_privilege(current_user,'device_push_tokens','DELETE') AND NOT has_table_privilege(current_user,'device_push_tokens','UPDATE,TRUNCATE')") == t ]]
  [[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT NOT has_table_privilege('baby_tracker_maintenance','device_push_tokens','SELECT,INSERT,UPDATE,DELETE,TRUNCATE') AND NOT has_any_column_privilege('baby_tracker_maintenance','device_push_tokens','SELECT,INSERT,UPDATE,REFERENCES')") == t ]]
  [[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT bool_and(has_function_privilege('baby_tracker_maintenance',f,'EXECUTE') AND NOT has_function_privilege('public',f,'EXECUTE') AND NOT has_function_privilege('baby_tracker_runtime',f,'EXECUTE')) FROM unnest(ARRAY['app.push_targets(uuid[],uuid[])','app.prune_push_tokens(integer)','app.forget_push_token(uuid,bytea)','app.ensure_audit_partitions(timestamp with time zone)','app.purge_family_step(uuid,integer)']) f") == t ]]
  [[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT to_regclass('public.rate_limit_buckets') IS NULL AND to_regprocedure('app.take_rate_limits(jsonb,timestamp with time zone)') IS NULL") == t ]]
fi
if (( expected_schema_version >= 35 )); then
  # Audit retention (000035, owner decision D1): the drop is a maintenance-only
  # definer; the runtime only reads the retention anchors.
  [[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT has_function_privilege('baby_tracker_maintenance',f,'EXECUTE') AND NOT has_function_privilege('public',f,'EXECUTE') AND NOT has_function_privilege('baby_tracker_runtime',f,'EXECUTE') FROM (SELECT 'app.drop_expired_audit_partition(timestamp with time zone)' AS f) d") == t ]]
  [[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT has_table_privilege('baby_tracker_runtime','audit_retention_anchors','SELECT') AND NOT has_table_privilege('baby_tracker_runtime','audit_retention_anchors','INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') AND NOT bool_or(has_table_privilege(r,'audit_retention_anchors','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')) FROM unnest(ARRAY['public','baby_tracker_maintenance']) r") == t ]]
fi
if (( expected_schema_version >= 32 )); then
  # Live Activity push (000032): the app registers its own devices and card
  # tokens (SELECT, INSERT and UPDATE, each on its own; never DELETE) and alone
  # runs the five registration definers; the maintenance group (the Live
  # Activity worker) holds nothing on any Live Activity table and alone runs
  # the nine worker definers; the scheduling tables are definer-only.
  [[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT bool_and(has_table_privilege(current_user,t,'SELECT') AND has_table_privilege(current_user,t,'INSERT') AND has_table_privilege(current_user,t,'UPDATE') AND NOT has_table_privilege(current_user,t,'DELETE') AND NOT has_table_privilege(current_user,t,'TRUNCATE')) FROM unnest(ARRAY['live_activity_devices','live_activity_tokens']) t") == t ]]
  [[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT NOT bool_or(has_table_privilege(r,t,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR has_any_column_privilege(r,t,'SELECT,INSERT,UPDATE,REFERENCES')) FROM unnest(ARRAY['public','baby_tracker_maintenance']) r CROSS JOIN unnest(ARRAY['live_activity_devices','live_activity_tokens','live_activity_token_owners','live_activity_registration_index','live_activity_family_dirty','live_activity_delivery','live_activity_installations','live_activity_history']) t") == t ]]
  [[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT NOT bool_or(has_table_privilege('baby_tracker_runtime',t,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR has_any_column_privilege('baby_tracker_runtime',t,'SELECT,INSERT,UPDATE,REFERENCES')) FROM unnest(ARRAY['live_activity_token_owners','live_activity_registration_index','live_activity_family_dirty','live_activity_delivery','live_activity_installations','live_activity_history']) t") == t ]]
  [[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT bool_and(has_function_privilege('baby_tracker_runtime',f,'EXECUTE') AND NOT has_function_privilege('public',f,'EXECUTE') AND NOT has_function_privilege('baby_tracker_maintenance',f,'EXECUTE')) FROM unnest(ARRAY['app.fence_live_activity_installation(uuid,bigint,uuid,uuid,uuid,uuid,bigint,timestamp with time zone,boolean)','app.live_activity_session_binding(uuid,uuid,uuid,bigint)','app.live_activity_claim_live(jsonb)','app.live_activity_registration_state(uuid,jsonb)','app.live_activity_authorized(uuid,bigint,boolean)']) f") == t ]]
  [[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT bool_and(has_function_privilege('baby_tracker_maintenance',f,'EXECUTE') AND NOT has_function_privilege('public',f,'EXECUTE') AND NOT has_function_privilege('baby_tracker_runtime',f,'EXECUTE')) FROM unnest(ARRAY['app.claim_live_activity_work(timestamp with time zone,integer)','app.finish_live_activity_work(jsonb,timestamp with time zone,text)','app.retain_live_activity_work()','app.live_activity_target(jsonb,jsonb,boolean,timestamp with time zone,jsonb,jsonb)','app.abandon_live_activity_start(jsonb,text,text,boolean,boolean)','app.release_stale_live_activity_lease(jsonb)','app.dirty_live_activity_families(uuid[],bigint[])','app.sweep_live_activity_work()','app.sweep_live_activity_registrations(boolean,integer)']) f") == t ]]
fi
# The deployment's roles are exactly these five: no projector or Apple-revoker
# role or login (Phase 4 Task 27).
[[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT string_agg(rolname, ',' ORDER BY rolname) FROM pg_roles WHERE rolname LIKE 'baby\_tracker\_%'") == baby_tracker_app,baby_tracker_export_app,baby_tracker_maintenance,baby_tracker_migrator,baby_tracker_runtime ]]
# server_state is written only by `authority init|rotate-epoch` (migrator).
[[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT has_table_privilege(current_user, 'server_state', 'SELECT') AND NOT has_table_privilege(current_user, 'server_state', 'INSERT') AND NOT has_table_privilege(current_user, 'server_state', 'UPDATE') AND NOT has_table_privilege(current_user, 'server_state', 'DELETE')") == t ]]
[[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*) FROM server_state WHERE instance_id='$BT_SERVER_INSTANCE_ID' AND recovery_epoch=1 AND octet_length(actor_key_commitment)=32") == 1 ]]
if docker exec "$db" psql -X -v ON_ERROR_STOP=1 -U baby_tracker_app -d babytracker -c 'CREATE TABLE public.runtime_must_not_own(id int)' >/dev/null 2>&1; then
  echo 'runtime unexpectedly has DDL privileges' >&2
  exit 1
fi
compose run --rm --no-deps migrate
# Also prove that real migration-granted runtime-group privileges are compatible.
docker exec -e BT_PRINCIPAL_NEGATIVE_PROBES=0 -i "$db" sh -s < Server/scripts/check-principals.sh
docker exec "$db" /docker-entrypoint-initdb.d/20-init-users.sh >/dev/null
# A second member of the runtime group is role drift: readiness refuses.
# The cached readiness audits roles at startup, after a schema change and
# every five minutes (requests never run it), so the node is restarted to
# audit now; once the drift is revoked the failed audit's retry, on the
# monitor's next tick, recovers without a restart.
docker exec "$db" psql -X -v ON_ERROR_STOP=1 -U postgres -d babytracker -c \
  'CREATE ROLE smoke_rogue LOGIN; GRANT baby_tracker_runtime TO smoke_rogue' >/dev/null
recreate_app
wait_for_ready_status 503
docker exec "$db" psql -X -v ON_ERROR_STOP=1 -U postgres -d babytracker -c \
  'REVOKE baby_tracker_runtime FROM smoke_rogue; DROP ROLE smoke_rogue' >/dev/null
wait_for_ready_status 200
# Prove a column-only grant is accepted only while the current exact snapshot says so.
docker exec "$db" psql -X -v ON_ERROR_STOP=1 -U postgres -d babytracker -c "
  INSERT INTO schema_privilege_manifest VALUES ($expected_schema_version,'baby_tracker_runtime','column','public.families.created_at','UPDATE');
  GRANT UPDATE (created_at) ON public.families TO baby_tracker_runtime;" >/dev/null
docker exec "$db" /docker-entrypoint-initdb.d/20-init-users.sh >/dev/null
docker exec "$db" psql -X -v ON_ERROR_STOP=1 -U postgres -d babytracker -c \
  'GRANT REFERENCES (id) ON public.families TO baby_tracker_runtime' >/dev/null
recreate_app
wait_for_ready_status 503
docker exec "$db" psql -X -v ON_ERROR_STOP=1 -U postgres -d babytracker -c "
  REVOKE REFERENCES (id) ON public.families FROM baby_tracker_runtime;
  REVOKE UPDATE (created_at) ON public.families FROM baby_tracker_runtime;
  DELETE FROM schema_privilege_manifest WHERE schema_version=$expected_schema_version AND object_kind='column' AND object_identity='public.families.created_at';" >/dev/null
docker exec "$db" /docker-entrypoint-initdb.d/20-init-users.sh >/dev/null
wait_for_ready_status 200
# The full authority verification walks every audit chain and binds the key.
verified=$(compose run --rm --no-deps migrate authority verify)
[[ $verified == "instance_id=$BT_SERVER_INSTANCE_ID epoch=1 "*" failed=0 result=verified" ]]
echo "post-migration proof: readiness 200; authority verify clean; exact server identity; baseline schema $expected_schema_version; migrator table ownership; runtime structural DML and migration writes denied; no projector or Apple-revoker principal; migrate/bootstrap reruns and current-version column snapshot succeeds; unmanifested drift rejected"
