#!/usr/bin/env bash
set -euo pipefail

if [[ -n ${BABY_TRACKER_TEST_DATABASE_URL:-} ]]; then
  echo "refusing external BABY_TRACKER_TEST_DATABASE_URL; server-test owns its disposable database" >&2
  exit 2
fi

for tool in docker openssl; do
  if ! command -v "$tool" >/dev/null; then
    echo "test-db requires $tool" >&2
    exit 2
  fi
done

cd "$(dirname "$0")/../.."
umask 077
project="baby-tracker-test-$(date +%s)-$$-$(openssl rand -hex 4)"
export BT_SECRETS_DIR
BT_SECRETS_DIR=$(mktemp -d "${TMPDIR:-/tmp}/baby-tracker-test.XXXXXXXX")
compose_file="$BT_SECRETS_DIR/compose.yaml"

compose() {
  docker compose --project-name "$project" -f "$compose_file" "$@"
}

cleanup() {
  local status=$?
  local cleanup_ok=1
  local remaining_containers=""
  local remaining_volumes=""
  trap - EXIT
  set +e
  compose down --volumes --remove-orphans >/dev/null 2>&1 || cleanup_ok=0
  remaining_containers=$(docker ps -aq --filter "label=com.docker.compose.project=$project") || cleanup_ok=0
  remaining_volumes=$(docker volume ls -q --filter "label=com.docker.compose.project=$project") || cleanup_ok=0
  if [[ $cleanup_ok -eq 0 || -n $remaining_containers || -n $remaining_volumes ]]; then
    echo "test-db cleanup FAILED for project $project" >&2
    [[ $status -ne 0 ]] || status=1
  fi
  if [[ $BT_SECRETS_DIR == "${TMPDIR:-/tmp}/baby-tracker-test."* && -d $BT_SECRETS_DIR ]]; then
    find "$BT_SECRETS_DIR" -type f -delete
    rmdir "$BT_SECRETS_DIR" || { [[ $status -ne 0 ]] || status=1; }
  else
    echo "test-db refused unsafe secret cleanup path" >&2
    [[ $status -ne 0 ]] || status=1
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for secret in bootstrap_password migrator_password runtime_password projector_password; do
  openssl rand -hex 32 > "$BT_SECRETS_DIR/$secret"
done
chmod 0600 "$BT_SECRETS_DIR"/*

printf '%s\n' \
  'services:' \
  '  db:' \
  '    image: postgres:17.6-alpine@sha256:ef257d85f76e48da1c64832459b59fcaba1a4dac97bf5d7450c77753542eee94' \
  '    environment:' \
  '      POSTGRES_USER: postgres' \
  '      POSTGRES_DB: babytracker' \
  '      POSTGRES_PASSWORD_FILE: /run/secrets/bootstrap_password' \
  '    secrets: [bootstrap_password]' \
  '    volumes: [database:/var/lib/postgresql/data]' \
  '    healthcheck:' \
  '      test: [CMD-SHELL, "pg_isready -h 127.0.0.1 -U postgres -d babytracker"]' \
  '      interval: 2s' \
  '      timeout: 3s' \
  '      retries: 30' \
  '    ports:' \
  '      - "127.0.0.1:0:5432"' \
  'volumes:' \
  '  database:' \
  'secrets:' \
  '  bootstrap_password:' \
  "    file: \${BT_SECRETS_DIR:?set BT_SECRETS_DIR}/bootstrap_password" > "$compose_file"
chmod 0600 "$compose_file"

compose config --quiet
compose up -d --wait --wait-timeout 120 db
db=$(compose ps -q db)
binding=$(compose port db 5432)
if [[ $binding != 127.0.0.1:* ]]; then
  echo "test-db PostgreSQL binding is not loopback-only: $binding" >&2
  exit 1
fi
host_port=${binding##*:}
if [[ ! $host_port =~ ^[0-9]+$ || $host_port == 0 ]]; then
  echo "test-db could not determine PostgreSQL port" >&2
  exit 1
fi

docker exec -i "$db" psql -X -U postgres -d babytracker -v ON_ERROR_STOP=1 < Server/deploy/postgres/init-roles.sql
docker exec "$db" psql -XAt -U postgres -d babytracker -v ON_ERROR_STOP=1 -c \
  "SELECT bool_and(pg_roles.oid IS NOT NULL) FROM (VALUES ('baby_tracker_runtime'),('baby_tracker_maintenance'),('baby_tracker_projector'),('baby_tracker_apple_revoker'),('baby_tracker_live_activity')) expected(rolname) LEFT JOIN pg_roles ON pg_roles.rolname=expected.rolname" | grep -Fx t >/dev/null
docker exec "$db" psql -XAt -U postgres -d babytracker -v ON_ERROR_STOP=1 -c \
  "SELECT NOT EXISTS (SELECT 1 FROM pg_auth_members membership JOIN pg_roles member ON member.oid=membership.member WHERE member.rolname IN ('baby_tracker_app','baby_tracker_export_app','baby_tracker_projector_app','baby_tracker_apple_revoker_app','baby_tracker_live_activity_app'))" | grep -Fx t >/dev/null

export BABY_TRACKER_REQUIRE_DATABASE=1
bootstrap_password=$(<"$BT_SECRETS_DIR/bootstrap_password")
export BABY_TRACKER_TEST_DATABASE_URL="postgres://postgres:$bootstrap_password@127.0.0.1:$host_port/babytracker?sslmode=disable"
go_test=(go test -p=1 -race -count=1)
if [[ -n ${BT_TEST_TAGS:-} ]]; then
  go_test+=(-tags "$BT_TEST_TAGS")
fi
if [[ -n ${BT_TEST_RUN:-} ]]; then
  go_test+=(-run "$BT_TEST_RUN")
fi
if [[ -n ${BT_TEST_PACKAGES:-} ]]; then
  read -r -a test_packages <<< "$BT_TEST_PACKAGES"
  go_test+=("${test_packages[@]}")
else
  go_test+=(./...)
fi
(cd Server && "${go_test[@]}")
