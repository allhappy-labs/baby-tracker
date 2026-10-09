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
  wait_for_ready_status 503
  if verification=$(compose run --rm --no-deps migrate security verify 2>&1); then
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
    "$BT_SECRETS_DIR/runtime_password" "$BT_SECRETS_DIR/projector_password" "$BT_SECRETS_DIR/export_password" "$BT_SECRETS_DIR/migrator_database_url" \
    "$BT_SECRETS_DIR/runtime_database_url" "$BT_SECRETS_DIR/projector_database_url" \
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
for secret in bootstrap_password migrator_password runtime_password projector_password export_password identity_pepper session_token_pepper device_continuity_pepper apple_notification_pepper invitation_pepper page_token_key actor_id_pepper rate_limit_pepper; do
  openssl rand -hex 32 > "$BT_SECRETS_DIR/$secret"
done
printf 'postgres://baby_tracker_migrator:%s@db:5432/babytracker?sslmode=disable\n' \
  "$(<"$BT_SECRETS_DIR/migrator_password")" > "$BT_SECRETS_DIR/migrator_database_url"
printf 'postgres://baby_tracker_app:%s@db:5432/babytracker?sslmode=disable\n' \
  "$(<"$BT_SECRETS_DIR/runtime_password")" > "$BT_SECRETS_DIR/runtime_database_url"
printf 'postgres://baby_tracker_projector_app:%s@db:5432/babytracker?sslmode=disable\n' \
  "$(<"$BT_SECRETS_DIR/projector_password")" > "$BT_SECRETS_DIR/projector_database_url"
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
docker inspect "$app" | jq -e '.[0].Mounts | map(select(.Type == "volume") | .Name) | length == 2 and (unique | length == 2)' >/dev/null
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
compose run --rm --no-deps migrate security init --instance-id "$BT_SERVER_INSTANCE_ID" --epoch 1 --confirm-instance "$BT_SERVER_INSTANCE_ID"
compose run --rm --no-deps -e BT_DATABASE_URL_FILE=/run/secrets/runtime_database_url migrate security reconcile
echo "migration completed; checking readiness"
ready=0
for _ in $(seq 1 100); do
  if [[ $(request /readyz) == 200 ]]; then ready=1; break; fi
  sleep 0.1
done
[[ $ready == 1 ]]
[[ $(request /v1/server) == 200 ]]
jq . "$BT_SECRETS_DIR/response.json"
jq -e --slurpfile expected Server/api/fixtures/server-info.json '
  .instance_id == "123e4567-e89b-12d3-a456-426614174000" and .recovery_epoch == 1 and
  (del(.instance_id,.recovery_epoch) == ($expected[0] | del(.instance_id,.recovery_epoch)))
' "$BT_SECRETS_DIR/response.json" >/dev/null
challenge_status=$(curl --silent --show-error --max-time 5 -o "$BT_SECRETS_DIR/response.json" -w '%{http_code}' \
  -H 'Content-Type: application/json' -d '{"protocol_version":"v1","capabilities":["identity-v1"]}' "$base/v1/auth/challenge")
[[ $challenge_status == 200 ]]
jq -e '.protocol_version == "v1" and (.challenge | length) == 43 and .server_instance_id == "123e4567-e89b-12d3-a456-426614174000" and .canonical_origin == "https://sync.example.test"' "$BT_SECRETS_DIR/response.json" >/dev/null
smoke_rejects_actor_id_pepper_replacement
echo 'actor-ID proof: identical restart stays ready; replacement returns actor_id_key_mismatch and readiness 503; restored key recovers readiness'
[[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tableowner <> 'baby_tracker_migrator'") == 0 ]]
[[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT count(*) > 0 FROM pg_tables WHERE schemaname='public' AND tableowner = 'baby_tracker_migrator'") == t ]]
expected_schema_version=$(jq -r '.max' Server/internal/migrate/compatibility.json)
[[ $expected_schema_version =~ ^[1-9][0-9]*$ ]]
[[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c 'SELECT current_version FROM schema_compatibility WHERE singleton') == "$expected_schema_version" ]]
[[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c 'SELECT count(*)=current_version AND min(version)=1 AND max(version)=current_version FROM schema_migrations CROSS JOIN schema_compatibility WHERE singleton GROUP BY current_version') == t ]]
[[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT has_table_privilege(current_user, 'schema_migrations', 'INSERT,UPDATE,DELETE,TRUNCATE')") == f ]]
[[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT has_table_privilege(current_user, 'families', 'INSERT,UPDATE,DELETE,TRUNCATE')") == f ]]
if (( expected_schema_version >= 9 )); then
  [[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT has_column_privilege(current_user,'families','next_sequence','UPDATE') AND NOT has_table_privilege(current_user,'families','UPDATE') AND NOT has_column_privilege(current_user,'families','status','UPDATE') AND NOT has_column_privilege(current_user,'families','display_name','UPDATE') AND NOT has_column_privilege(current_user,'families','purge_after','UPDATE')") == t ]]
  [[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT bool_and(has_table_privilege(current_user,t,'SELECT,INSERT,UPDATE')) FROM unnest(ARRAY['babies','feed_entries','sleep_entries','diaper_entries']) t") == t ]]
  # 000027 dropped the feed side and food tables.
  if (( expected_schema_version < 27 )); then
    [[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT bool_and(has_table_privilege(current_user,t,'SELECT,INSERT,DELETE') AND NOT has_table_privilege(current_user,t,'UPDATE,TRUNCATE')) FROM unnest(ARRAY['feed_side_intervals','feed_foods']) t") == t ]]
  fi
  [[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT has_table_privilege(current_user,'mutation_receipts','SELECT') AND NOT has_table_privilege(current_user,'mutation_receipts','INSERT,UPDATE,DELETE') AND has_table_privilege(current_user,'mutation_journal','SELECT,INSERT') AND NOT has_table_privilege(current_user,'mutation_journal','UPDATE,DELETE')") == t ]]
  [[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT has_table_privilege(current_user,'family_record_usage','SELECT,INSERT,UPDATE') AND NOT has_table_privilege(current_user,'family_record_usage','DELETE,TRUNCATE')") == t ]]
fi
if (( expected_schema_version >= 13 )); then
  [[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT NOT has_table_privilege(current_user,'delivery_pending','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') AND has_function_privilege(current_user,'app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)','EXECUTE')") == t ]]
  [[ $(docker exec "$db" psql -XAt -U baby_tracker_projector_app -d babytracker -c "SELECT NOT has_table_privilege(current_user,'delivery_pending','SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') AND NOT has_function_privilege(current_user,'app.schedule_family_wakes(uuid,uuid,uuid,bigint,bigint)','EXECUTE')") == t ]]
fi
[[ $(docker exec "$db" psql -XAt -U baby_tracker_app -d babytracker -c "SELECT has_function_privilege(current_user, 'app.update_server_state(uuid,bigint,bigint,bigint,bytea)', 'EXECUTE')") == f ]]
[[ $(docker exec "$db" psql -XAt -U baby_tracker_projector_app -d babytracker -c "SELECT has_function_privilege(current_user, 'app.update_server_state(uuid,bigint,bigint,bigint,bytea)', 'EXECUTE')") == t ]]
[[ $(docker exec "$db" psql -XAt -U baby_tracker_projector_app -d babytracker -c "SELECT has_table_privilege(current_user, 'accounts', 'SELECT,INSERT,UPDATE,DELETE')") == f ]]
[[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT rolpassword IS NULL FROM pg_authid WHERE rolname='baby_tracker_apple_revoker_app'") == t ]]
[[ $(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT pg_has_role('baby_tracker_apple_revoker_app','baby_tracker_apple_revoker','MEMBER')") == t ]]
if docker exec "$db" psql -X -v ON_ERROR_STOP=1 -U baby_tracker_app -d babytracker -c 'CREATE TABLE public.runtime_must_not_own(id int)' >/dev/null 2>&1; then
  echo 'runtime unexpectedly has DDL privileges' >&2
  exit 1
fi
compose run --rm --no-deps migrate
# Also prove that real migration-granted runtime-group privileges are compatible.
docker exec -e BT_PRINCIPAL_NEGATIVE_PROBES=0 -i "$db" sh -s < Server/scripts/check-principals.sh
docker exec "$db" /docker-entrypoint-initdb.d/20-init-users.sh >/dev/null
docker exec "$db" psql -X -v ON_ERROR_STOP=1 -U postgres -d babytracker -c \
  'CREATE ROLE smoke_rogue LOGIN; GRANT baby_tracker_projector TO smoke_rogue' >/dev/null
wait_for_ready_status 503
docker exec "$db" psql -X -v ON_ERROR_STOP=1 -U postgres -d babytracker -c \
  'REVOKE baby_tracker_projector FROM smoke_rogue; DROP ROLE smoke_rogue' >/dev/null
wait_for_ready_status 200
# Prove a column-only grant is accepted only while the current exact snapshot says so.
docker exec "$db" psql -X -v ON_ERROR_STOP=1 -U postgres -d babytracker -c "
  INSERT INTO schema_privilege_manifest VALUES ($expected_schema_version,'baby_tracker_runtime','column','public.families.display_name','UPDATE');
  GRANT UPDATE (display_name) ON public.families TO baby_tracker_runtime;" >/dev/null
docker exec "$db" /docker-entrypoint-initdb.d/20-init-users.sh >/dev/null
docker exec "$db" psql -X -v ON_ERROR_STOP=1 -U postgres -d babytracker -c \
  'GRANT REFERENCES (id) ON public.families TO baby_tracker_runtime' >/dev/null
wait_for_ready_status 503
docker exec "$db" psql -X -v ON_ERROR_STOP=1 -U postgres -d babytracker -c "
  REVOKE REFERENCES (id), UPDATE (display_name) ON public.families FROM baby_tracker_runtime;
  DELETE FROM schema_privilege_manifest WHERE schema_version=$expected_schema_version AND object_kind='column' AND object_identity='public.families.display_name';" >/dev/null
docker exec "$db" /docker-entrypoint-initdb.d/20-init-users.sh >/dev/null
wait_for_ready_status 200
echo "post-migration proof: readiness 200; exact server identity; contiguous schema $expected_schema_version; migrator table ownership; runtime structural DML and migration writes denied; passwordless self-hosted revoker principal; migrate/bootstrap reruns and current-version column snapshot succeeds; unmanifested drift rejected"
