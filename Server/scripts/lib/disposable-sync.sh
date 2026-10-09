#!/usr/bin/env bash
# Owned disposable composition shared by protocol and performance runners.
# Call from repository root. Caller may replace EXIT trap, but must call stop.
stop_disposable_sync() {
  local status=${1:-0}
  local cleanup_ok=1
  local remaining_containers=""
  local remaining_volumes=""
  trap - EXIT
  set +e
  if [[ $status -ne 0 ]]; then
    compose logs --no-color --tail 100 app >&2 || true
  fi
  if [[ ${BT_PROTOCOL_DIAGNOSTICS:-0} == 1 && ${BT_RECOVERY_E2E_MODE:-} == self_hosted ]]; then
    if [[ -n ${protocol_metrics_pid:-} ]]; then
      kill -TERM "$protocol_metrics_pid" 2>/dev/null || true
      wait "$protocol_metrics_pid" 2>/dev/null || true
    fi
    python3 Server/scripts/lib/performance_control.py sample-summary "$BT_SECRETS_DIR/protocol-resource-samples.jsonl"
    compose logs --no-color --tail 10000 app 2>/dev/null | python3 Server/scripts/lib/performance_control.py smoke-summary
  fi
  compose down --volumes --remove-orphans >/dev/null 2>&1 || cleanup_ok=0
  remaining_containers=$(docker ps -aq --filter "label=com.docker.compose.project=$project") || cleanup_ok=0
  remaining_volumes=$(docker volume ls -q --filter "label=com.docker.compose.project=$project") || cleanup_ok=0
  if docker image inspect "$BT_IMAGE_NAME" >/dev/null 2>&1; then
    docker image rm "$BT_IMAGE_NAME" >/dev/null 2>&1 || cleanup_ok=0
  fi
  if [[ $cleanup_ok -eq 0 || -n $remaining_containers || -n $remaining_volumes ]]; then
    echo "protocol-e2e cleanup FAILED for $project" >&2
    [[ $status -ne 0 ]] || status=1
  else
    echo "cleanup proof: $project has zero containers and zero volumes"
  fi
  if [[ $BT_SECRETS_DIR == "${TMPDIR:-/tmp}/baby-tracker-protocol."* && -d $BT_SECRETS_DIR ]]; then
    for process_state in "$BT_SECRETS_DIR"/recovery-*/process.json; do
      [[ -f $process_state ]] || continue
      while IFS= read -r suite; do
        if [[ $suite == recovery-owner-* || $suite == recovery-caregiver-* ]]; then
          defaults delete "$suite" >/dev/null 2>&1 || true
        else
          echo "refused unexpected recovery defaults domain" >&2
          [[ $status -ne 0 ]] || status=1
        fi
      done < <(jq -r '.ownerSuite, .caregiverSuite' "$process_state")
    done
    find "$BT_SECRETS_DIR" -type f -delete
    find "$BT_SECRETS_DIR" -mindepth 1 -depth -type d -delete
    rmdir "$BT_SECRETS_DIR" || { [[ $status -ne 0 ]] || status=1; }
  else
    echo "protocol-e2e refused unsafe secret cleanup path" >&2
    [[ $status -ne 0 ]] || status=1
  fi
  return "$status"
}

compose() {
  if [[ ${BT_DISPOSABLE_PERFORMANCE:-0} == 1 ]]; then
    # Shell BT_* isolation alone does not stop Compose's implicit dotenv input.
    # Explicitly select this run's empty file and ignore inherited file selectors.
    env -u COMPOSE_ENV_FILES -u COMPOSE_DISABLE_ENV_FILE \
      docker compose --env-file "$BT_SECRETS_DIR/compose.env" \
      --project-name "$project" -f Server/compose.yaml -f "$override" "$@" &
    disposable_command_pid=$!
    local command_status=0
    wait "$disposable_command_pid" || command_status=$?
    disposable_command_pid=""
    return "$command_status"
  else
    docker compose --project-name "$project" -f Server/compose.yaml -f "$override" ${live_override:+-f "$live_override"} "$@"
  fi
}

start_disposable_sync() {
  [[ $# == 2 && $2 == normal ]] || { echo "unsupported disposable build profile" >&2; return 2; }
if [[ -n ${BABY_TRACKER_TEST_DATABASE_URL:-} ]]; then
  echo "protocol-e2e refuses an externally supplied database; it owns disposable application and PostgreSQL volumes" >&2
  exit 2
fi
for tool in docker curl jq openssl go git shasum python3; do
  command -v "$tool" >/dev/null || { echo "protocol-e2e requires $tool" >&2; exit 2; }
done

umask 077
project="baby-tracker-protocol-$(date +%s)-$$-$(openssl rand -hex 4)"
export BT_SECRETS_DIR
BT_SECRETS_DIR=$(mktemp -d "${TMPDIR:-/tmp}/baby-tracker-protocol.XXXXXXXX")
if [[ ${BT_DISPOSABLE_PERFORMANCE:-0} == 1 ]]; then
  export BT_KEYRING_DIR="$BT_SECRETS_DIR/keyrings"
  mkdir -m 0700 "$BT_KEYRING_DIR"
  : > "$BT_SECRETS_DIR/compose.env"
fi
override="$BT_SECRETS_DIR/compose.override.yaml"
state="$BT_SECRETS_DIR/client-state.json"
binary="$BT_SECRETS_DIR/baby-tracker-server"
source_sha=$(git rev-parse HEAD)
export BT_IMAGE_NAME="$project:identitytest"
export BT_SERVER_INSTANCE_ID=70000000-0000-4000-8000-000000000001
export BT_RECOVERY_EPOCH=1 BT_APPLE_AUDIENCE=com.example.babytracker.protocol
# The TLS front's port is chosen up front so the server's public origin can
# be the very origin the iOS client dials. The client binds Apple's nonce to
# the origin it confirmed and the server derives it from its own; with the
# two different (the server believing it is sync.example.test while the
# client dials 127.0.0.1) no real sign-in could succeed, and every invitation
# link named a host the client had never confirmed. `localhost`, because the
# server refuses an IP-literal origin.
tls_port=""
for _ in $(seq 1 50); do
  candidate=$((20000 + RANDOM % 40000))
  if ! (exec 3<>"/dev/tcp/127.0.0.1/$candidate") 2>/dev/null; then tls_port=$candidate; break; fi
done
[[ -n $tls_port ]] || { echo "protocol-e2e found no free port for the TLS front" >&2; exit 2; }
case "$1" in
  hosted) export BT_MODE=hosted BT_IDENTITYTEST_HOSTED_ADAPTER=1 ;;
  self_hosted) export BT_MODE=self_hosted BT_IDENTITYTEST_HOSTED_ADAPTER=0 ;;
  *) echo "invalid recovery mode" >&2; exit 2 ;;
esac
export BT_PUBLIC_ORIGIN="https://localhost:$tls_port"
export BT_HTTP_PORT=0 BT_TRUSTED_PROXY_CIDRS=127.0.0.1/32


if [[ ${BT_DISPOSABLE_PERFORMANCE:-0} != 1 ]]; then
  trap 'disposable_status=$?; trap - EXIT; stop_disposable_sync "$disposable_status"; exit $?' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
fi

for secret in bootstrap_password migrator_password runtime_password projector_password export_password identity_pepper session_token_pepper device_continuity_pepper apple_notification_pepper invitation_pepper page_token_key actor_id_pepper rate_limit_pepper; do
  openssl rand -hex 32 > "$BT_SECRETS_DIR/$secret"
done
printf 'postgres://baby_tracker_migrator:%s@db:5432/babytracker?sslmode=disable\n' "$(<"$BT_SECRETS_DIR/migrator_password")" > "$BT_SECRETS_DIR/migrator_database_url"
printf 'postgres://baby_tracker_app:%s@db:5432/babytracker?sslmode=disable\n' "$(<"$BT_SECRETS_DIR/runtime_password")" > "$BT_SECRETS_DIR/runtime_database_url"
printf 'postgres://baby_tracker_projector_app:%s@db:5432/babytracker?sslmode=disable\n' "$(<"$BT_SECRETS_DIR/projector_password")" > "$BT_SECRETS_DIR/projector_database_url"
chmod 0444 "$BT_SECRETS_DIR"/*_password "$BT_SECRETS_DIR"/*_pepper "$BT_SECRETS_DIR"/*_url "$BT_SECRETS_DIR/page_token_key"

# A self-signed leaf for 127.0.0.1, and an nginx in front of the app to
# terminate it.
#
# Not decoration. `ServerOrigin` refuses any scheme but https — correctly, and
# it is not going to be relaxed for a test — so the Swift client at the end of
# this script cannot be pointed at a plain-HTTP container at all. The Swift
# side pins this exact certificate rather than disabling validation, so what
# runs is the real client against real TLS, and a swapped certificate fails.
openssl req -x509 -newkey rsa:2048 -sha256 -days 1 -nodes \
  -keyout "$BT_SECRETS_DIR/tls.key" -out "$BT_SECRETS_DIR/tls.crt" \
  -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" >/dev/null 2>&1
chmod 0444 "$BT_SECRETS_DIR/tls.crt" "$BT_SECRETS_DIR/tls.key"
#
# It passes WebSocket upgrades through, as Tailscale Serve does in the real
# deployment: without the Upgrade and Connection headers (and HTTP/1.1 to the
# app, which nginx does not use by default) every `/v1/live` dial reached the
# app as a plain GET and was refused, so the live socket could never be proved
# here. conf.d is included in the http context, which is where a map belongs.
printf '%s\n' \
  'map $http_upgrade $connection_upgrade {' \
  '  default upgrade;' \
  "  '' close;" \
  '}' \
  'server {' \
  '  listen 8443 ssl;' \
  '  ssl_certificate /etc/nginx/tls.crt;' \
  '  ssl_certificate_key /etc/nginx/tls.key;' \
  '  location / {' \
  '    proxy_pass http://app:8080;' \
  '    proxy_http_version 1.1;' \
  '    proxy_set_header Upgrade $http_upgrade;' \
  '    proxy_set_header Connection $connection_upgrade;' \
  '    proxy_set_header Host $host;' \
  '    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;' \
  '  }' \
  '}' > "$BT_SECRETS_DIR/nginx.conf"
chmod 0444 "$BT_SECRETS_DIR/nginx.conf"

printf '%s\n' \
  'services:' \
  '  db:' \
  '    ports: ["127.0.0.1:0:5432"]' \
  '  app:' \
	'    environment:' \
  "      BT_IDENTITYTEST_HOSTED_ADAPTER: \"$BT_IDENTITYTEST_HOSTED_ADAPTER\"" \
  '    build:' \
  '      context: .' \
  '      args:' \
  '        BT_BUILD_TAGS: identitytest' \
  "        BT_SOURCE_SHA: $source_sha" \
  '  migrate:' \
  '    build:' \
  '      context: .' \
  '      args:' \
  '        BT_BUILD_TAGS: identitytest' \
  "        BT_SOURCE_SHA: $source_sha" \
  '  tls:' \
  '    image: nginx:1.27-alpine@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10' \
  '    depends_on: [app]' \
  "    ports: [\"127.0.0.1:$tls_port:8443\"]" \
  '    volumes:' \
  "      - $BT_SECRETS_DIR/tls.crt:/etc/nginx/tls.crt:ro" \
  "      - $BT_SECRETS_DIR/tls.key:/etc/nginx/tls.key:ro" \
  "      - $BT_SECRETS_DIR/nginx.conf:/etc/nginx/conf.d/default.conf:ro" > "$override"
if [[ -n ${BT_RECOVERY_E2E_MODE:-} || ${BT_DISPOSABLE_PERFORMANCE:-0} == 1 ]]; then
  printf 'postgres://baby_tracker_export_app:%s@db:5432/babytracker?sslmode=disable\n' "$(<"$BT_SECRETS_DIR/export_password")" > "$BT_SECRETS_DIR/recovery_database_url"
  chmod 0444 "$BT_SECRETS_DIR/recovery_database_url"
  export BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE=/run/secrets/recovery_database_url
  # Merge a narrowly owned mount without changing production compose secrets.
  printf '\n' >> "$override"
  python3 - "$override" "$BT_SECRETS_DIR/recovery_database_url" <<'RECOVERY_MOUNT'
import sys
p=sys.argv[1]
s=open(p).read()
s=s.replace('  app:\n', '  app:\n    volumes:\n      - '+sys.argv[2]+':/run/secrets/recovery_database_url:ro\n',1)
open(p,'w').write(s)
RECOVERY_MOUNT
fi
if [[ ${BT_DISPOSABLE_PERFORMANCE:-0} == 1 || (${BT_PROTOCOL_DIAGNOSTICS:-0} == 1 && ${BT_RECOVERY_E2E_MODE:-} == self_hosted) ]]; then
  python3 - "$override" "${BT_SYNC_PERF_TEST_FAULT:-}" <<'PERF_OVERRIDE'
import sys
p=sys.argv[1]; s=open(p).read()
s=s.replace('    environment:\n','    environment:\n      BT_SYNC_DIAGNOSTICS: "1"\n',1)
if sys.argv[2] == 'start_failure':
    s=s.replace('  app:\n','  app:\n    entrypoint: [/nonexistent-performance-start-failure]\n',1)
open(p,'w').write(s)
PERF_OVERRIDE
fi
chmod 0600 "$override"

# Optional Live Activity push profile (BT_DISPOSABLE_LIVE_ACTIVITY=1): the
# production worker, role-separated pool and APNs transport, with a disposable
# P-256 signing key and Apple's hosts replaced by the identitytest recorder
# (BT_IDENTITYTEST_APNS_SINK). Nothing here is an Apple credential and nothing
# can reach Apple. Off for performance runs, so their baselines are unchanged.
live_override=""
if [[ ${BT_DISPOSABLE_LIVE_ACTIVITY:-0} == 1 && ${BT_DISPOSABLE_PERFORMANCE:-0} != 1 ]]; then
  live_override="$BT_SECRETS_DIR/compose.live-activity.yaml"
  openssl rand -hex 32 > "$BT_SECRETS_DIR/live_activity_password"
  printf 'postgres://baby_tracker_live_activity_app:%s@db:5432/babytracker?sslmode=disable\n' "$(<"$BT_SECRETS_DIR/live_activity_password")" > "$BT_SECRETS_DIR/live_activity_database_url"
  openssl ecparam -name prime256v1 -genkey -noout 2>/dev/null | openssl pkcs8 -topk8 -nocrypt -out "$BT_SECRETS_DIR/apns_key.p8"
  # PostgreSQL's init reads the password; the server requires 0600 for the
  # worker URL and the key (Docker Desktop maps bind-mount ownership).
  chmod 0444 "$BT_SECRETS_DIR/live_activity_password"
  chmod 0600 "$BT_SECRETS_DIR/live_activity_database_url" "$BT_SECRETS_DIR/apns_key.p8"
  printf '%s\n' \
    'services:' \
    '  db:' \
    '    secrets: [live_activity_password]' \
    '  app:' \
    '    environment:' \
    '      BT_LIVE_ACTIVITY_PUSH_ENABLED: "true"' \
    '      BT_LIVE_ACTIVITY_DATABASE_URL_FILE: /run/secrets/live_activity_database_url' \
    '      BT_APNS_TEAM_ID: E2ETEAM001' \
    '      BT_APNS_KEY_ID: E2EKEY0001' \
    '      BT_APNS_BUNDLE_ID: com.example.babytracker.protocol' \
    '      BT_APNS_KEY_FILE: /run/secrets/apns_key' \
    '      BT_IDENTITYTEST_APNS_SINK: "1"' \
    '    secrets: [live_activity_database_url, apns_key]' \
    'secrets:' \
    '  live_activity_password:' \
    "    file: $BT_SECRETS_DIR/live_activity_password" \
    '  live_activity_database_url:' \
    "    file: $BT_SECRETS_DIR/live_activity_database_url" \
    '  apns_key:' \
    "    file: $BT_SECRETS_DIR/apns_key.p8" > "$live_override"
  chmod 0600 "$live_override"
fi

compose config --quiet
compose up -d --wait --wait-timeout 120 db
compose build app
compose run --rm --no-deps migrate
compose run --rm --no-deps migrate security init --instance-id "$BT_SERVER_INSTANCE_ID" --epoch "$BT_RECOVERY_EPOCH" --confirm-instance "$BT_SERVER_INSTANCE_ID"
compose run --rm --no-deps -e BT_DATABASE_URL_FILE=/run/secrets/runtime_database_url migrate security reconcile
[[ ${BT_DISPOSABLE_PERFORMANCE:-0} != 1 ]] || phase=app_start
if [[ ${BT_SYNC_PERF_TEST_FAULT:-} == interrupt ]]; then kill -INT "$$"; fi
if [[ ${BT_SYNC_PERF_TEST_FAULT:-} == timeout ]]; then
  sleep 60 & disposable_command_pid=$!
  wait "$disposable_command_pid"
  echo "timeout watchdog failed" >&2
  return 1
fi
compose up -d app

app=$(compose ps -q app)
db=$(compose ps -q db)
binding=""
for _ in $(seq 1 100); do
  binding=$(compose port app 8080 2>/dev/null || true)
  if [[ $binding == 127.0.0.1:* ]] && curl --silent --fail --max-time 1 "http://$binding/readyz" >/dev/null; then
    break
  fi
  sleep 0.1
done
[[ $binding == 127.0.0.1:* ]]
curl --silent --fail --max-time 2 "http://$binding/readyz" >/dev/null
base="http://$binding"

image_id=$(docker inspect --format '{{.Image}}' "$app")
image_arch=$(docker image inspect --format '{{.Architecture}}' "$BT_IMAGE_NAME")
image_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$BT_IMAGE_NAME")
docker cp "$app:/usr/local/bin/baby-tracker-server" "$binary"
binary_sha=$(shasum -a 256 "$binary" | awk '{print $1}')
schema=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c 'SELECT current_version FROM schema_compatibility WHERE singleton')
authority=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT instance_id::text||':'||recovery_epoch::text FROM server_state WHERE singleton")
# A literal, bumped by hand with every migration that moves the compatibility
# version. It is deliberately not `>=`: this gate exists to prove the image was
# built from THIS tree, and a range would pass against a future schema the
# client in this tree has never seen.
#
# It was left at 13 when 000014_viewer_role.sql landed, which is the migration
# the read-only caregiver needs. `set -e` and a bare `[[ ]]` mean that failure
# printed nothing at all — the run built the image, migrated, started the
# server, answered /readyz twice, tore everything down and exited non-zero
# without a word. So the two-client gate had not actually run since the
# caregiver work began.
#
# 29 is 000029_baby_icon.sql. The embedded compatibility manifest and
# the database's own privilege manifest must both name it too, so the literal,
# the image and the tree cannot drift apart silently.
manifest=$(jq -r '"\(.min):\(.max):\(.known[-1].name)"' Server/internal/migrate/compatibility.json)
privilege_manifest=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c 'SELECT max(schema_version) FROM schema_privilege_manifest')
if [[ $schema != 29 || $manifest != "29:29:000029_baby_icon.sql" || $privilege_manifest != 29 ]]; then
  echo "disposable server schema=$schema manifest=$manifest privilege_manifest=$privilege_manifest; this gate expects 29" >&2
  exit 1
fi
[[ $authority == "$BT_SERVER_INSTANCE_ID:$BT_RECOVERY_EPOCH" && $image_revision == "$source_sha" ]]
live_activity_capability=$(curl --silent --fail --max-time 2 "$base/v1/server" | jq -r '.capabilities | index("live-activity-push-v1") != null')
if [[ -n $live_override ]]; then
  [[ $live_activity_capability == true ]] || { echo "push profile did not advertise live-activity-push-v1" >&2; exit 1; }
else
  [[ $live_activity_capability == false ]] || { echo "push-disabled profile advertised live-activity-push-v1" >&2; exit 1; }
fi
echo "image proof: id=$image_id source_sha=$image_revision binary_sha=$binary_sha goarch=$image_arch schema=$schema manifest=$manifest authority=$authority live_activity_push=$live_activity_capability"
if [[ ${BT_PROTOCOL_DIAGNOSTICS:-0} == 1 && ${BT_RECOVERY_E2E_MODE:-} == self_hosted ]]; then
  python3 Server/scripts/lib/performance_control.py sample-loop "$app" "$db" "$BT_SECRETS_DIR/protocol-resource-samples.jsonl" & protocol_metrics_pid=$!
fi


}
