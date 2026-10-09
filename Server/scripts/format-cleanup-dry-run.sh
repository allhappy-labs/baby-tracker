#!/usr/bin/env bash
# Format cleanup dry-run (runbook S-4): migrate a restored copy of the
# production database with the new server image, entirely on this host.
#
#   Server/scripts/format-cleanup-dry-run.sh <pg_dump -Fc file> <new server image>
#
# It starts a disposable PostgreSQL container from the image digest pinned in
# Server/compose.yaml, on its own internal network with no published ports,
# initialised by the same init-roles/init-users scripts compose mounts. It
# restores the dump as `postgres`, runs format-cleanup-preflight.sql, runs
# `baby-tracker-admin migrate` from the given image as baby_tracker_migrator
# (as `compose run --rm --no-deps migrate` does in production), then runs
# format-cleanup-postflight.sql against the preflight's numbers.
#
# Output is limited to counts, PASS/FAIL lines and the session listing. The
# container, network, volume and temporary credentials are removed on every
# exit, including failures. Needs only bash and docker (plus coreutils).
set -euo pipefail
umask 077

usage() {
  echo "usage: $0 <pg_dump -Fc file> <new server image (tag or id)>" >&2
  exit 2
}
fail() {
  echo "format-cleanup-dry-run: $*" >&2
  exit 1
}

[[ $# -eq 2 ]] || usage
dump=$1
server_image=$2
[[ -f $dump && -r $dump ]] || fail "dump is not a readable file: $dump"
command -v docker >/dev/null || fail "docker is required"

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
server_dir=$(cd "$script_dir/.." && pwd)
init_roles=$server_dir/deploy/postgres/init-roles.sql
init_users=$server_dir/deploy/postgres/init-users.sh
preflight=$script_dir/format-cleanup-preflight.sql
postflight=$script_dir/format-cleanup-postflight.sql
for file in "$init_roles" "$init_users" "$preflight" "$postflight" "$server_dir/compose.yaml"; do
  [[ -f $file ]] || fail "missing $file"
done

# The same PostgreSQL image, by digest, as production's db service.
postgres_image=$(sed -n 's/^[[:space:]]*image:[[:space:]]*\(postgres:[^[:space:]]*@sha256:[0-9a-f]\{64\}\)[[:space:]]*$/\1/p' "$server_dir/compose.yaml" | head -n 1)
[[ -n $postgres_image ]] || fail "no digest-pinned postgres image in $server_dir/compose.yaml"

docker image inspect "$server_image" >/dev/null 2>&1 || fail "server image not found locally: $server_image (build it first)"
server_revision=$(docker image inspect --format '{{with .Config.Labels}}{{index . "org.opencontainers.image.revision"}}{{end}}' "$server_image")
[[ $server_revision != "<no value>" ]] || server_revision=""
server_image_id=$(docker image inspect --format '{{.Id}}' "$server_image")

# Every resource is named after this run and labelled with it; none may
# collide with the production compose project or with anything that exists.
run_id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
prefix="bt-format-cleanup-dry-run-$run_id"
label="org.babytracker.format-cleanup-dry-run=$run_id"
db_container="$prefix-db"
migrate_container="$prefix-migrate"
network="$prefix-net"
volume="$prefix-data"
for name in "$db_container" "$migrate_container" "$network" "$volume"; do
  case $name in
    *baby-tracker-sync*) fail "refusing a name that belongs to the production project: $name" ;;
  esac
  if docker container inspect "$name" >/dev/null 2>&1 || docker network inspect "$name" >/dev/null 2>&1 \
     || docker volume inspect "$name" >/dev/null 2>&1; then
    fail "refusing to reuse an existing docker resource: $name"
  fi
done

work=$(mktemp -d "${TMPDIR:-/tmp}/bt-format-cleanup-dry-run.XXXXXXXX")
created_network=0
created_volume=0

cleanup() {
  local status=$?
  local leftovers=""
  # A second Ctrl-C must not cut the cleanup short.
  trap '' INT TERM
  trap - EXIT
  set +e
  docker rm -f "$migrate_container" >/dev/null 2>&1
  docker rm -f -v "$db_container" >/dev/null 2>&1
  [[ $created_network -eq 1 ]] && docker network rm "$network" >/dev/null 2>&1
  [[ $created_volume -eq 1 ]] && docker volume rm "$volume" >/dev/null 2>&1
  leftovers=$( { docker ps -aq --filter "label=$label"; docker network ls -q --filter "label=$label"; docker volume ls -q --filter "label=$label"; } 2>&1)
  if [[ $work == "${TMPDIR:-/tmp}/bt-format-cleanup-dry-run."* && -d $work ]]; then
    rm -rf "$work"
  fi
  if [[ -n $leftovers || -e $work ]]; then
    echo "CLEANUP: FAIL (left behind for run $run_id: ${leftovers:-temporary directory})" >&2
    [[ $status -ne 0 ]] || status=1
  else
    echo "CLEANUP: removed container, network, volume and temporary credentials of run $run_id"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

secret() { od -An -N32 -tx1 /dev/urandom | tr -d ' \n'; }
for name in bootstrap_password migrator_password runtime_password projector_password export_password; do
  secret > "$work/$name"
done
printf 'postgres://baby_tracker_migrator:%s@db:5432/babytracker?sslmode=disable\n' "$(<"$work/migrator_password")" \
  > "$work/migrator_database_url"
# Read by the containers' own users (postgres 70, server 65532), as the
# provisioned production secrets are; the directory itself stays 0700.
chmod 0444 "$work"/*

echo "== Format cleanup dry-run $run_id"
echo "postgres image: $postgres_image"
echo "server image:   $server_image_id (revision ${server_revision:-unlabelled})"

docker network create --internal --label "$label" "$network" >/dev/null
created_network=1
docker volume create --label "$label" "$volume" >/dev/null
created_volume=1

secret_mounts=()
for name in bootstrap_password migrator_password runtime_password projector_password export_password; do
  secret_mounts+=(-v "$work/$name:/run/secrets/$name:ro")
done
docker run -d --name "$db_container" --label "$label" --network "$network" --network-alias db \
  -e POSTGRES_USER=postgres -e POSTGRES_DB=babytracker -e POSTGRES_PASSWORD_FILE=/run/secrets/bootstrap_password \
  "${secret_mounts[@]}" -v "$volume:/var/lib/postgresql/data" \
  -v "$init_roles:/docker-entrypoint-initdb.d/10-init-roles.sql:ro" \
  -v "$init_users:/docker-entrypoint-initdb.d/20-init-users.sh:ro" \
  "$postgres_image" >/dev/null

# The entrypoint's temporary init server listens on the socket only, so a TCP
# answer means the init scripts finished and the real server is up.
ready=0
for _ in $(seq 1 120); do
  if [[ $(docker inspect --format '{{.State.Running}}' "$db_container" 2>/dev/null) != true ]]; then
    break
  fi
  if docker exec "$db_container" pg_isready -q -h 127.0.0.1 -U postgres -d babytracker 2>/dev/null; then
    ready=1
    break
  fi
  sleep 1
done
if [[ $ready -ne 1 ]]; then
  # Nothing is restored yet, so the log holds only the server's own init output.
  docker logs --tail 30 "$db_container" >&2 2>&1 || true
  fail "disposable PostgreSQL did not become ready (init-roles/init-users failed?)"
fi

echo "== Restore"
# pg_restore's errors can quote row data (DETAIL, CONTEXT, COPY input), so
# they stay in the temporary directory; only the error count and the first
# error, cut before any quoted or parenthesised value, are printed.
if ! docker exec -i "$db_container" pg_restore -U postgres -d babytracker --single-transaction --exit-on-error \
     < "$dump" > "$work/restore.log" 2>&1; then
  echo "pg_restore: $(grep -c 'error:' "$work/restore.log" || true) error line(s); first: $(grep -m 1 'error:' "$work/restore.log" | sed 's/[("].*//')" >&2
  fail "pg_restore failed"
fi

psql_in_db() {
  docker exec -i "$db_container" psql -X -q -v ON_ERROR_STOP=1 -U postgres -d babytracker "$@"
}

echo "== Preflight"
preflight_status=0
psql_in_db < "$preflight" > "$work/preflight.out" 2>&1 || preflight_status=$?
grep -v '^FORMAT_CLEANUP_BEFORE ' "$work/preflight.out" || true
[[ $preflight_status -eq 0 ]] || fail "preflight failed (exit $preflight_status); not migrating"
before=$(grep '^FORMAT_CLEANUP_BEFORE ' "$work/preflight.out" || true)
[[ $(printf '%s\n' "$before" | grep -c .) -eq 1 ]] || fail "preflight printed no FORMAT_CLEANUP_BEFORE line"

echo "== Migrate (baby-tracker-admin migrate as baby_tracker_migrator)"
docker run --rm --name "$migrate_container" --label "$label" --network "$network" \
  --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  -e BT_DATABASE_URL_FILE=/run/secrets/migrator_database_url \
  -v "$work/migrator_database_url:/run/secrets/migrator_database_url:ro" \
  --entrypoint /usr/local/bin/baby-tracker-admin "$server_image" migrate \
  || fail "migrate failed"
echo "migrate: exit 0"

echo "== Postflight"
psql_in_db -v before="$before" < "$postflight" || fail "postflight failed"

echo "DRY-RUN: PASS"
