#!/usr/bin/env bash
# Live Activity push upgrade rehearsal (runbook R1-3b): run the exact R1
# cutover, 27 -> 28 with the init-users.sh rerun, against a disposable copy
# of the production database, entirely on this host, and remove everything.
#
# Historical: this is the 27 -> 28 rehearsal. From migration 000029 on
# (000029_baby_icon adds `babies.icon`), `migrate` changes the `babies` table,
# so this script is expected to STOP on the `babies` row digest in its
# "every care table unchanged" check. That STOP is not a regression; the
# logic is kept as it was for the record.
#
#   Server/scripts/rehearse-live-activity-upgrade.sh \
#     --dump <pg_dump -Fc file> --ledger-tar <security-ledger + ledger-head tar> \
#     --actor-id-pepper <file> [--old-tree <dir>] [--old-image <tag>] \
#     [--new-tree <dir>] [--image <tag>]
#   Server/scripts/rehearse-live-activity-upgrade.sh --synthetic [same options]
#
# --dump      the R1-2 backup (pg_dump -Fc of babytracker), with the ledger
#             and head directories tarred at the same point and the deployment's
#             actor_id_pepper. The file contents are never printed.
# --synthetic builds the old tree's image, seeds a realistic schema-27 family
#             through its real API and the fixtures the format-cleanup tests
#             use, dumps it exactly as R1-6 does, and rehearses on that.
#
# The old tree is the deployed source (default: `git archive` of main at
# 8032e79, the production revision); the new tree is the staged source
# (default: the tree this script lives in). Images are built from the trees
# unless given; given images are not removed.
#
# Steps, each a PASS line or a STOP: restore into a disposable PostgreSQL that
# the OLD tree's init scripts created (the R1-2 restore path); row counts and
# content digests of every table; recreate db on the NEW init scripts and
# rerun init-users.sh (R1-6 step 2, exact roles); `migrate` (step 3, schema
# exactly 29, contiguous, digest of 28); privilege inventory exactly the manifest's
# (init-users.sh rerun, check-principals.sh); the old image refusing schema 28
# (its `migrate` and its server's readiness); the new image ready with push off
# (step 4, /readyz 200, no capability, identity unchanged); R2's database
# half (worker password mounted, init-users.sh rerun,
# `live-activity-preflight --skip-apple`); every care table unchanged.
#
# Output holds counts, names, hashes and PASS/STOP lines only. Passwords are
# generated for the run and never printed. Containers, volumes, networks,
# built images and the temporary directory are owned by this run (a unique
# compose project) and removed on every exit, including failures; the run
# fails if anything is left behind. Needs bash, docker (compose), openssl, jq,
# curl, tar; --synthetic also needs go and (without --old-tree) git.
set -euo pipefail
umask 077

usage() {
  cat >&2 <<'USAGE'
usage: rehearse-live-activity-upgrade.sh --dump <file> --ledger-tar <file> --actor-id-pepper <file> [options]
       rehearse-live-activity-upgrade.sh --synthetic [options]
options: --old-tree <dir>   deployed source (contains Server/); default: git archive of --old-rev
         --old-rev <rev>    default 8032e7972cbc107a0576e4e104c781de81ea7ba5 (main, schema 27)
         --old-image <tag>  built old image (default: built from the old tree, removed afterwards)
         --new-tree <dir>   staged source (contains Server/); default: this script's tree
         --image <tag>      built new image (default: built from the new tree, removed afterwards)
         --work-dir <dir>   parent of the run's temporary directory (default: $TMPDIR or /tmp; on Unraid
                            /tmp is RAM-backed, so pass a disk path such as the deployment root)
USAGE
  exit 2
}
fail() {
  echo "STOP: $*" >&2
  exit 1
}

mode="" dump="" ledger_tar="" pepper_file="" old_tree="" old_image="" new_tree="" new_image="" work_root=""
old_rev=8032e7972cbc107a0576e4e104c781de81ea7ba5
while [[ $# -gt 0 ]]; do
  case $1 in
    --dump) [[ $# -ge 2 ]] || usage; [[ -z $mode ]] || { echo 'either --dump or --synthetic, not both' >&2; usage; }; mode=dump; dump=$2; shift 2 ;;
    --synthetic) [[ -z $mode ]] || { echo 'either --dump or --synthetic, not both' >&2; usage; }; mode=synthetic; shift ;;
    --ledger-tar) [[ $# -ge 2 ]] || usage; ledger_tar=$2; shift 2 ;;
    --actor-id-pepper) [[ $# -ge 2 ]] || usage; pepper_file=$2; shift 2 ;;
    --old-tree) [[ $# -ge 2 ]] || usage; old_tree=$2; shift 2 ;;
    --old-rev) [[ $# -ge 2 ]] || usage; old_rev=$2; shift 2 ;;
    --old-image) [[ $# -ge 2 ]] || usage; old_image=$2; shift 2 ;;
    --new-tree) [[ $# -ge 2 ]] || usage; new_tree=$2; shift 2 ;;
    --image) [[ $# -ge 2 ]] || usage; new_image=$2; shift 2 ;;
    --work-dir) [[ $# -ge 2 ]] || usage; work_root=$2; shift 2 ;;
    *) usage ;;
  esac
done
[[ -n $mode ]] || usage
readable() { [[ -f $1 && -r $1 && ! -L $1 ]]; }
if [[ $mode == dump ]]; then
  readable "$dump" || { echo "--dump is not a readable file: $dump" >&2; exit 2; }
  [[ -n $ledger_tar ]] || { echo '--dump needs --ledger-tar (security-ledger and ledger-head from the same point)' >&2; exit 2; }
  readable "$ledger_tar" || { echo "--ledger-tar is not a readable file: $ledger_tar" >&2; exit 2; }
  [[ -n $pepper_file ]] || { echo "--dump needs --actor-id-pepper (the deployment's actor_id_pepper; readiness binds to it)" >&2; exit 2; }
  readable "$pepper_file" || { echo "--actor-id-pepper is not a readable file" >&2; exit 2; }
else
  [[ -z $ledger_tar && -z $pepper_file ]] || { echo '--ledger-tar and --actor-id-pepper are used only with --dump' >&2; exit 2; }
  [[ -z $old_image ]] || { echo '--synthetic builds its own old image (the seeding client needs its identitytest adapter); --old-image is used only with --dump' >&2; exit 2; }
fi
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
[[ -n $new_tree ]] || new_tree=$(cd "$script_dir/../.." && pwd)
[[ -d $new_tree/Server && -f $new_tree/Server/compose.yaml ]] || { echo "new tree has no Server/compose.yaml: $new_tree" >&2; exit 2; }
if [[ -n $old_tree ]]; then
  [[ -d $old_tree/Server && -f $old_tree/Server/compose.yaml ]] || { echo "old tree has no Server/compose.yaml: $old_tree" >&2; exit 2; }
fi
if [[ -n $work_root ]]; then
  [[ -d $work_root && -w $work_root ]] || { echo "--work-dir is not a writable directory: $work_root" >&2; exit 2; }
  # Absolute and physical from here on: the run directory under it is
  # bind-mounted into containers and matched again on cleanup, and neither may
  # depend on the caller's working directory or on a symlinked path (Unraid's
  # /mnt/user is one).
  work_root=$(cd "$work_root" && pwd -P)
fi
# The bind-mounted ledger and the secret files are chowned to the containers'
# UIDs (65532, 70); on Linux only root can do that. Docker Desktop and
# OrbStack map bind-mount ownership, so macOS needs no root.
if [[ $(uname -s) == Linux && $(id -u) -ne 0 ]]; then
  echo 'on Linux the rehearsal must run as root (it chowns the bind-mounted ledger and secret files to UID 65532 and 70)' >&2
  exit 2
fi

for tool in docker openssl jq curl tar; do
  command -v "$tool" >/dev/null || fail "$tool is required"
done
docker compose version >/dev/null 2>&1 || fail "docker compose is required"
if [[ $mode == synthetic ]]; then
  command -v go >/dev/null || fail "--synthetic needs go (the seeding client runs from the old tree)"
fi
if [[ -z $old_tree ]]; then
  command -v git >/dev/null || fail "git is required to archive the old tree (or pass --old-tree)"
  git -C "$new_tree" rev-parse --verify --quiet "$old_rev^{commit}" >/dev/null || fail "old revision $old_rev is not in $new_tree (pass --old-tree or --old-rev)"
fi
sha256_file() { openssl dgst -sha256 -r "$1" | cut -d' ' -f1; }
# The listing is read in full before it is searched. `tar -tf | grep -q` lets
# grep leave at the first match; on a listing longer than the pipe buffer GNU
# tar then dies of SIGPIPE and pipefail turns a good tar into a false STOP
# (production's ledger tar did, while the small synthetic one passed).
check_ledger_tar() { # tar
  local listing
  listing=$(tar -tf "$1") || fail "ledger tar is not readable: $1"
  grep -q '^security-ledger/' <<<"$listing" || fail "ledger tar has no security-ledger/"
  grep -q '^ledger-head/' <<<"$listing" || fail "ledger tar has no ledger-head/"
}

# Variables the compose files read must come from this run's env files only:
# an exported BT_IMAGE_NAME or BT_SOURCE_SHA would override them (the
# runbook's hazard), so every BT_* and COMPOSE_* variable goes, whatever its name.
isolate_compose_environment() {
  local name
  for name in $(compgen -v BT_) $(compgen -v COMPOSE_); do
    unset "$name"
  done
}
isolate_compose_environment

# One compose project per run; everything is labelled with it. Never the
# production project's name.
run_id="$(date -u +%Y%m%dT%H%M%SZ)-$$-$(openssl rand -hex 3)"
project="bt-rehearse-$(date +%s)-$$-$(openssl rand -hex 4)"
case $project in *baby-tracker-sync*) fail "refusing a production project name" ;; esac
if [[ -n $(docker ps -aq --filter "label=com.docker.compose.project=$project") ]]; then
  fail "refusing to reuse an existing compose project: $project"
fi
[[ -n $work_root ]] || work_root=${TMPDIR:-/tmp}
work_root=${work_root%/}
work=$(mktemp -d "$work_root/bt-rehearse.XXXXXXXX")
owned_images=()

compose_down() {
  docker compose --project-name "$project" --env-file "$work/deployment.new.env" \
    -f "$new_tree/Server/compose.yaml" -f "$work/compose.override.yaml" down --volumes --remove-orphans >/dev/null 2>&1
}
cleanup() {
  local status=$?
  local leftovers="" image
  # A second Ctrl-C must not cut the cleanup short.
  trap '' INT TERM
  trap - EXIT
  set +e
  if [[ -n ${project:-} ]]; then
    compose_down
    # Whatever compose left (a half-created project, a different file set): by label.
    for id in $(docker ps -aq --filter "label=com.docker.compose.project=$project"); do docker rm -f "$id" >/dev/null 2>&1; done
    for id in $(docker volume ls -q --filter "label=com.docker.compose.project=$project"); do docker volume rm "$id" >/dev/null 2>&1; done
    for id in $(docker network ls -q --filter "label=com.docker.compose.project=$project"); do docker network rm "$id" >/dev/null 2>&1; done
    for image in ${owned_images[@]+"${owned_images[@]}"}; do
      [[ -n $(docker image ls -q --filter "reference=$image") ]] && docker image rm "$image" >/dev/null 2>&1
    done
    leftovers=$( {
      docker ps -aq --filter "label=com.docker.compose.project=$project"
      docker volume ls -q --filter "label=com.docker.compose.project=$project"
      docker network ls -q --filter "label=com.docker.compose.project=$project"
      for image in ${owned_images[@]+"${owned_images[@]}"}; do docker image ls -q --filter "reference=$image"; done
    } 2>&1 | grep -v '^$')
  fi
  if [[ -n ${work:-} && $work == "$work_root/bt-rehearse."* && -d $work ]]; then
    chmod -R u+rwX "$work" 2>/dev/null
    rm -rf "$work"
  fi
  if [[ -n $leftovers || ( -n ${work:-} && -e $work ) ]]; then
    echo "CLEANUP: FAIL (left behind for run ${run_id:-?}: ${leftovers:-temporary directory})" >&2
    [[ $status -ne 0 ]] || status=1
  else
    echo "CLEANUP: removed containers, volumes, network, built images and the temporary directory of run ${run_id:-?}"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -m 0700 "$work/secrets" "$work/logs" "$work/ledger" "$work/seed"
secrets="$work/secrets"
echo "== Live Activity push upgrade rehearsal $run_id ($mode)"
echo "project: $project"

# ---------------------------------------------------------------------------
# Trees and manifests. The old tree must predate the roles and accept exactly
# 27; the new tree must accept exactly 28 and carry the role and overlay.
# ---------------------------------------------------------------------------
if [[ -z $old_tree ]]; then
  old_sha=$(git -C "$new_tree" rev-parse "$old_rev^{commit}")
  mkdir "$work/old"
  git -C "$new_tree" archive --format=tar "$old_sha" Server | tar -x -C "$work/old"
  old_tree="$work/old"
  echo "old tree: git archive $old_sha"
else
  old_sha=$old_rev
  echo "old tree: $old_tree"
fi
new_sha=$(git -C "$new_tree" rev-parse HEAD 2>/dev/null || true)
[[ -n $new_sha ]] || new_sha="rehearsal-$run_id"
echo "new tree: $new_tree (source $new_sha)"

old_manifest=$(jq -c '{min,max}' "$old_tree/Server/internal/migrate/compatibility.json")
new_manifest=$(jq -c '{min,max}' "$new_tree/Server/internal/migrate/compatibility.json")
[[ $old_manifest == '{"min":27,"max":27}' ]] || fail "old tree manifest is $old_manifest, not {\"min\":27,\"max\":27} (R1-1)"
[[ $new_manifest == '{"min":29,"max":29}' ]] || fail "new tree manifest is $new_manifest, not {\"min\":29,\"max\":29} (R1-3)"
digest_28=$(jq -r '.known[] | select(.version == 28) | .sha256' "$new_tree/Server/internal/migrate/compatibility.json")
[[ $(jq -r '.known[] | select(.version == 28) | .name' "$new_tree/Server/internal/migrate/compatibility.json") == 000028_live_activity_push.sql ]] || fail "new tree's migration 28 is not 000028_live_activity_push.sql"
grep -q baby_tracker_live_activity "$new_tree/Server/deploy/postgres/init-roles.sql" || fail "new init-roles.sql lacks baby_tracker_live_activity (R1-3)"
! grep -q baby_tracker_live_activity "$old_tree/Server/deploy/postgres/init-roles.sql" || fail "old init-roles.sql already names baby_tracker_live_activity; this is not the pre-upgrade tree"
push_overlay="$new_tree/Server/deploy/unraid/live-activity-push.yaml"
[[ -f $push_overlay ]] || fail "new tree lacks deploy/unraid/live-activity-push.yaml (R1-3)"
postgres_image=$(sed -n 's/^[[:space:]]*image:[[:space:]]*\(postgres:[^[:space:]]*@sha256:[0-9a-f]\{64\}\)[[:space:]]*$/\1/p' "$new_tree/Server/compose.yaml" | head -n 1)
[[ -n $postgres_image ]] || fail "no digest-pinned postgres image in the new compose.yaml"
[[ $(sed -n 's/^[[:space:]]*image:[[:space:]]*\(postgres:[^[:space:]]*@sha256:[0-9a-f]\{64\}\)[[:space:]]*$/\1/p' "$old_tree/Server/compose.yaml" | head -n 1) == "$postgres_image" ]] \
  || echo "note: the old tree pins a different postgres image; the rehearsal uses the new tree's $postgres_image"
echo "manifests: old $old_manifest, new $new_manifest, 28 = $digest_28"
echo "postgres image: $postgres_image"

# ---------------------------------------------------------------------------
# Secrets and composition. The production compose.yaml with the production
# push overlay; an override only for the build arguments and the bind-mounted
# ledger directories (as the Unraid overlay mounts them).
# ---------------------------------------------------------------------------
secret() { openssl rand -hex 32; }
for name in bootstrap_password migrator_password runtime_password projector_password export_password \
            identity_pepper session_token_pepper device_continuity_pepper apple_notification_pepper \
            invitation_pepper page_token_key actor_id_pepper rate_limit_pepper; do
  secret > "$secrets/$name"
done
printf 'postgres://baby_tracker_migrator:%s@db:5432/babytracker?sslmode=disable\n' "$(<"$secrets/migrator_password")" > "$secrets/migrator_database_url"
printf 'postgres://baby_tracker_app:%s@db:5432/babytracker?sslmode=disable\n' "$(<"$secrets/runtime_password")" > "$secrets/runtime_database_url"
printf 'postgres://baby_tracker_projector_app:%s@db:5432/babytracker?sslmode=disable\n' "$(<"$secrets/projector_password")" > "$secrets/projector_database_url"
# Read by the containers' own users (postgres 70, server 65532), as the
# provisioned production secrets are; the directory itself stays 0700.
chmod 0444 "$secrets"/*

# Bind-mounted application state, as production's: owned by UID 65532 on
# Linux (root chowns, modes untouched: the actor-ID key sidecar must stay
# 0600); on Docker Desktop and OrbStack bind-mount ownership is mapped.
app_owned_dir() {
  mkdir -p "$1"
  if [[ $(id -u) -eq 0 ]]; then chown -R 65532:65532 "$1"; fi
}
write_override() {
  local file=$1 ledger_root=$2
  cat > "$file" <<OVERRIDE
services:
  app:
    build:
      args:
        BT_SOURCE_SHA: \${BT_SOURCE_SHA:?set BT_SOURCE_SHA}
        BT_BUILD_TAGS: \${BT_BUILD_TAGS:-}
  migrate:
    build:
      args:
        BT_SOURCE_SHA: \${BT_SOURCE_SHA:?set BT_SOURCE_SHA}
        BT_BUILD_TAGS: \${BT_BUILD_TAGS:-}
volumes:
  security_ledger:
    driver: local
    driver_opts:
      type: none
      o: bind
      device: $ledger_root/security-ledger
  ledger_head:
    driver: local
    driver_opts:
      type: none
      o: bind
      device: $ledger_root/ledger-head
OVERRIDE
  chmod 0600 "$file"
}
write_env() {
  # $1 file, $2 image, $3 source sha, $4 build tags, $5 instance id, $6 epoch
  cat > "$1" <<ENV
BT_SECRETS_DIR=$secrets
BT_DEPLOY_ROOT=$work
BT_IMAGE_NAME=$2
BT_SOURCE_SHA=$3
BT_BUILD_TAGS=$4
BT_MODE=self_hosted
BT_PUBLIC_ORIGIN=https://rehearsal.example.test
BT_SERVER_INSTANCE_ID=$5
BT_RECOVERY_EPOCH=$6
BT_APPLE_AUDIENCE=com.example.babytracker.rehearsal
BT_HTTP_PORT=0
BT_TRUSTED_PROXY_CIDRS=127.0.0.1/32
BT_IDENTITYTEST_HOSTED_ADAPTER=0
BT_APNS_TEAM_ID=REHEARSE01
BT_APNS_KEY_ID=REHEARSE02
BT_APNS_BUNDLE_ID=com.example.babytracker.rehearsal
ENV
  chmod 0600 "$1"
}
compose_with() {
  # $1 tree, $2 env file, $3 override, $4 push overlay or "", then arguments
  local tree=$1 env_file=$2 override=$3 push=$4
  shift 4
  docker compose --project-name "$project" --env-file "$env_file" \
    -f "$tree/Server/compose.yaml" -f "$override" ${push:+-f "$push"} "$@"
}
compose_old() { compose_with "$old_tree" "$work/deployment.old.env" "$work/compose.override.yaml" "" "$@"; }
compose_new() { compose_with "$new_tree" "$work/deployment.new.env" "$work/compose.override.yaml" "" "$@"; }
compose_push() { compose_with "$new_tree" "$work/deployment.new.env" "$work/compose.override.yaml" "$push_overlay" "$@"; }
compose_seed() { compose_with "$old_tree" "$work/deployment.seed.env" "$work/compose.seed.yaml" "" "$@"; }

db_container() { docker ps -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=db; }
db_psql() { docker exec -i "$(db_container)" psql -X -q -v ON_ERROR_STOP=1 -U postgres -d babytracker "$@"; }
schema() { db_psql -At -c 'SELECT current_version FROM schema_compatibility WHERE singleton'; }
revision() { docker image inspect --format '{{with .Config.Labels}}{{index . "org.opencontainers.image.revision"}}{{end}}' "$1"; }
app_base() {
  local binding
  binding=$(compose_new port app 8080 2>/dev/null || true)
  [[ $binding == 127.0.0.1:* ]] || binding=$(compose_old port app 8080)
  [[ $binding == 127.0.0.1:* ]] || fail "app port is not bound to loopback: $binding"
  echo "http://$binding"
}
request() {
  # $1 base, $2 path -> status code; body in $work/response.json
  local code
  code=$(curl --silent --connect-timeout 2 --max-time 5 -o "$work/response.json" -w '%{http_code}' "$1$2" 2>/dev/null) || code=000
  echo "${code:-000}"
}
wait_ready() {
  # $1 base, $2 expected status, $3 attempts (x 0.5 s)
  local observed=""
  for _ in $(seq 1 "$3"); do
    observed=$(request "$1" /readyz)
    [[ $observed == "$2" ]] && return 0
    sleep 0.5
  done
  echo "readiness remained $observed, wanted $2" >&2
  return 1
}
init_users_rerun() {
  # R1-6 step 2 / R2-6: the bootstrap as one transaction, through the new compose.
  local log="$work/logs/init-users-$1.log"
  if ! "$2" exec -T -u postgres db /docker-entrypoint-initdb.d/20-init-users.sh > "$log" 2>&1; then
    # The bootstrap's errors name roles and manifest rows, never data; print the first.
    echo "init-users.sh: $(grep -m 1 -E 'ERROR|FATAL' "$log" | sed 's/[("].*//')" >&2
    return 1
  fi
}
# Every table with its row count and a content digest, one line each.
inventory() {
  db_psql -At -F ' ' <<'SQL'
SELECT n.nspname||'.'||c.relname,
  (xpath('/row/n/text()', query_to_xml(format('SELECT count(*) AS n FROM %I.%I', n.nspname, c.relname), false, true, '')))[1]::text,
  (xpath('/row/d/text()', query_to_xml(format('SELECT md5(coalesce(string_agg(t::text, E''\n'' ORDER BY t::text), '''')) AS d FROM %I.%I t', n.nspname, c.relname), false, true, '')))[1]::text
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE c.relkind IN ('r','p') AND n.nspname !~ '^pg_' AND n.nspname<>'information_schema'
ORDER BY 1
SQL
}
# Compare two inventories; $3 is a space-separated list of tables allowed to
# change; new tables must be empty. Prints the differences it refuses.
compare_inventories() {
  local before=$1 after=$2 allowed=" $3 " table count digest before_line status=0
  while read -r table count digest; do
    before_line=$(grep "^$table " "$before" || true)
    if [[ -z $before_line ]]; then
      if [[ $count != 0 ]]; then echo "new table $table has $count rows" >&2; status=1; fi
      continue
    fi
    if [[ $before_line != "$table $count $digest" ]]; then
      if [[ $allowed != *" $table "* ]]; then
        echo "table $table changed (rows $(cut -d' ' -f2 <<< "$before_line") -> $count)" >&2
        status=1
      else
        echo "changed as expected: $table (rows $(cut -d' ' -f2 <<< "$before_line") -> $count)"
      fi
    fi
  done < "$after"
  while read -r table _; do
    grep -q "^$table " "$after" || { echo "table $table disappeared" >&2; status=1; }
  done < "$before"
  return $status
}
care_counts() {
  db_psql -At -F ' ' <<'SQL' | tr '\n' ' '
SELECT t||'='||(xpath('/row/n/text()', query_to_xml(format('SELECT count(*) AS n FROM public.%I', t), false, true, '')))[1]::text
FROM unnest(ARRAY['accounts','devices','sessions','families','memberships','babies','feed_entries','feed_components','sleep_entries','diaper_entries','active_timers','mutation_journal']) t
WHERE to_regclass('public.'||t) IS NOT NULL
SQL
  echo
}

# ---------------------------------------------------------------------------
# Images. Built from the trees unless given; built images are owned.
# ---------------------------------------------------------------------------
build_tags=""
[[ $mode == synthetic ]] && build_tags=identitytest   # the seeding client signs in through the identitytest adapter
placeholder_instance=00000000-0000-4000-8000-000000000000
write_override "$work/compose.override.yaml" "$work/ledger"
write_override "$work/compose.seed.yaml" "$work/seed/ledger"
if [[ -z $old_image ]]; then
  old_image="$project-old:rehearsal"
  owned_images+=("$old_image")
  write_env "$work/deployment.old.env" "$old_image" "$old_sha" "$build_tags" "$placeholder_instance" 1
  echo "== Build old image from $old_tree${build_tags:+ (tags: $build_tags)}"
  compose_old build app > "$work/logs/build-old.log" 2>&1 || { tail -n 20 "$work/logs/build-old.log" >&2; fail "old image build failed"; }
else
  docker image inspect "$old_image" >/dev/null 2>&1 || fail "old image not found locally: $old_image"
  write_env "$work/deployment.old.env" "$old_image" "$old_sha" "" "$placeholder_instance" 1
fi
if [[ -z $new_image ]]; then
  new_image="$project-new:rehearsal"
  owned_images+=("$new_image")
  write_env "$work/deployment.new.env" "$new_image" "$new_sha" "" "$placeholder_instance" 1
  echo "== Build new image from $new_tree"
  compose_new build app > "$work/logs/build-new.log" 2>&1 || { tail -n 20 "$work/logs/build-new.log" >&2; fail "new image build failed"; }
else
  docker image inspect "$new_image" >/dev/null 2>&1 || fail "new image not found locally: $new_image"
  write_env "$work/deployment.new.env" "$new_image" "$new_sha" "" "$placeholder_instance" 1
fi
old_revision=$(revision "$old_image"); new_revision=$(revision "$new_image")
echo "old image: $(docker image inspect --format '{{.Id}} {{.Os}}/{{.Architecture}}' "$old_image") revision ${old_revision:-unlabelled}"
echo "new image: $(docker image inspect --format '{{.Id}} {{.Os}}/{{.Architecture}}' "$new_image") revision ${new_revision:-unlabelled}"
[[ $(docker image inspect --format '{{.Id}}' "$old_image") != $(docker image inspect --format '{{.Id}}' "$new_image") ]] || fail "old and new images are the same image"

# ---------------------------------------------------------------------------
# --synthetic: a schema-27 deployment of the old tree, seeded through its
# real API (the protocol end-to-end client: four accounts and devices, a
# family, an invitation, a baby, a feed timer started and stopped into a
# completed feed, imported feeds) plus the format-cleanup fixtures (a
# multi-component feeding session, a sleep session, a diaper, a running sleep
# timer), then dumped exactly as R1-6 step 1 dumps production.
# ---------------------------------------------------------------------------
if [[ $mode == synthetic ]]; then
  h=$(openssl rand -hex 16)
  seed_instance="${h:0:8}-${h:8:4}-4${h:13:3}-8${h:17:3}-${h:20:12}"
  write_env "$work/deployment.seed.env" "$old_image" "$old_sha" "$build_tags" "$seed_instance" 1
  app_owned_dir "$work/seed/ledger/security-ledger"
  app_owned_dir "$work/seed/ledger/ledger-head"
  echo "== Synthetic schema-27 deployment (old tree, instance $seed_instance)"
  compose_seed up -d --wait --wait-timeout 120 db > "$work/logs/seed-db.log" 2>&1 || { tail -n 20 "$work/logs/seed-db.log" >&2; fail "old db did not start"; }
  compose_seed run --rm --no-deps migrate > "$work/logs/seed-migrate.log" 2>&1 || { tail -n 5 "$work/logs/seed-migrate.log" >&2; fail "old migrate failed"; }
  compose_seed run --rm --no-deps migrate security init --instance-id "$seed_instance" --epoch 1 --confirm-instance "$seed_instance" > "$work/logs/seed-security.log" 2>&1 \
    || { tail -n 5 "$work/logs/seed-security.log" >&2; fail "security init failed"; }
  compose_seed run --rm --no-deps -e BT_DATABASE_URL_FILE=/run/secrets/runtime_database_url migrate security reconcile >> "$work/logs/seed-security.log" 2>&1 \
    || { tail -n 5 "$work/logs/seed-security.log" >&2; fail "security reconcile failed"; }
  [[ $(schema) == 27 ]] || fail "old tree migrated to schema $(schema), not 27"
  compose_seed up -d --no-deps --wait --wait-timeout 120 app > "$work/logs/seed-app.log" 2>&1 || { tail -n 20 "$work/logs/seed-app.log" >&2; fail "old app did not start"; }
  base=$(compose_seed port app 8080); [[ $base == 127.0.0.1:* ]] || fail "old app port not on loopback"
  base="http://$base"
  wait_ready "$base" 200 120 || fail "old app never became ready"
  echo "old app ready at schema 27; seeding through its API"
  (cd "$old_tree/Server" && go run -tags identitytest ./cmd/protocol-e2e-client --base "$base" --state "$work/seed/state.json" --phase bootstrap) > "$work/logs/seed-client.log" 2>&1 \
    || { tail -n 5 "$work/logs/seed-client.log" >&2; fail "seeding (bootstrap) failed"; }
  (cd "$old_tree/Server" && go run -tags identitytest ./cmd/protocol-e2e-client --base "$base" --state "$work/seed/state.json" --phase verify) >> "$work/logs/seed-client.log" 2>&1 \
    || { tail -n 5 "$work/logs/seed-client.log" >&2; fail "seeding (verify) failed"; }
  db_psql < "$script_dir/rehearse-live-activity-upgrade-seed.sql" > "$work/logs/seed-fixtures.log" 2>&1 \
    || { awk '/ERROR|DETAIL|CONTEXT/ && ++shown <= 3 { sub(/\(.*/, ""); print }' "$work/logs/seed-fixtures.log" >&2; fail "seed fixtures failed"; }
  echo "seeded: $(care_counts)"
  # R1-6 step 1: stop only the app, dump, tar the ledger and head at one point.
  compose_seed stop app > /dev/null 2>&1
  dump="$work/seed/backup.cutover-rehearsal.dump"
  ledger_tar="$work/seed/backup.cutover-rehearsal.ledger.tar"
  docker exec "$(db_container)" pg_dump -Fc -U postgres -d babytracker > "$dump"
  tar -C "$work/seed/ledger" -cf "$ledger_tar" security-ledger ledger-head
  pepper_file="$secrets/actor_id_pepper"
  echo "dump: $(sha256_file "$dump") ($(docker exec -i "$(db_container)" pg_restore --list < "$dump" | wc -l | tr -d ' ') entries); ledger tar: $(sha256_file "$ledger_tar")"
  compose_seed down --volumes > "$work/logs/seed-down.log" 2>&1 || fail "could not remove the seeding composition"
  [[ -z $(docker ps -aq --filter "label=com.docker.compose.project=$project") && -z $(docker volume ls -q --filter "label=com.docker.compose.project=$project") ]] \
    || fail "seeding composition left containers or volumes"
  echo "SEED: OK (schema-27 dump and ledger tar from the old tree)"
fi

# ---------------------------------------------------------------------------
# Restore: the R1-2 rehearsed path, a fresh database that the OLD tree's init
# scripts created (as production's db container was), then pg_restore.
# ---------------------------------------------------------------------------
echo "== Restore into a fresh database initialised by the old tree"
echo "dump: $(sha256_file "$dump")"
check_ledger_tar "$ledger_tar"
if [[ $pepper_file != "$secrets/actor_id_pepper" ]]; then
  chmod 0600 "$secrets/actor_id_pepper"; cat "$pepper_file" > "$secrets/actor_id_pepper"; chmod 0444 "$secrets/actor_id_pepper"
fi
app_owned_dir "$work/ledger"
tar -C "$work/ledger" -xf "$ledger_tar" security-ledger ledger-head
app_owned_dir "$work/ledger"
compose_old up -d --no-deps --wait --wait-timeout 120 db > "$work/logs/restore-db.log" 2>&1 || { tail -n 20 "$work/logs/restore-db.log" >&2; fail "disposable db did not start on the old init scripts"; }
! compose_old exec -T db grep -q baby_tracker_live_activity /docker-entrypoint-initdb.d/10-init-roles.sql || fail "db was created with the new init scripts"
# pg_restore's errors can quote row data, so they stay in the temporary directory.
if ! docker exec -i "$(db_container)" pg_restore -U postgres -d babytracker --single-transaction --exit-on-error < "$dump" > "$work/logs/restore.log" 2>&1; then
  echo "pg_restore: $(grep -c 'error:' "$work/logs/restore.log" || true) error line(s); first: $(grep -m 1 'error:' "$work/logs/restore.log" | sed 's/[("].*//')" >&2
  fail "pg_restore failed"
fi
[[ $(schema) == 27 ]] || fail "restored schema is $(schema), not 27 (R1-1)"
[[ $(db_psql -At -c 'SELECT count(*)=current_version AND min(version)=1 AND max(version)=current_version FROM schema_migrations CROSS JOIN schema_compatibility WHERE singleton GROUP BY current_version') == t ]] || fail "restored migrations are not contiguous"
[[ $(db_psql -At -c 'SELECT count(*) FROM schema_migrations WHERE version = 28') == 0 ]] || fail "restored database already has migration 28"
[[ $(db_psql -At -c 'SELECT max(schema_version) FROM schema_privilege_manifest') == 27 ]] || fail "restored privilege manifest is not 27"
[[ $(db_psql -At -c "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='babytracker'") == baby_tracker_migrator ]] || fail "restored database is not owned by baby_tracker_migrator"
[[ $(db_psql -At -c "SELECT count(*) FROM pg_tables WHERE schemaname='public' AND tableowner <> 'baby_tracker_migrator'") == 0 ]] || fail "restored tables are not all owned by baby_tracker_migrator"
read -r instance_id epoch < <(db_psql -At -F ' ' -c 'SELECT instance_id::text, recovery_epoch::text FROM server_state WHERE singleton')
[[ $instance_id =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ && $epoch =~ ^[1-9][0-9]*$ ]] || fail "restored server_state has no instance and epoch"
write_env "$work/deployment.old.env" "$old_image" "$old_sha" "$build_tags" "$instance_id" "$epoch"
write_env "$work/deployment.new.env" "$new_image" "$new_sha" "" "$instance_id" "$epoch"
inventory > "$work/inventory.27"
echo "restored: schema 27, instance $instance_id, epoch $epoch, $(wc -l < "$work/inventory.27" | tr -d ' ') tables; $(care_counts)"
echo "RESTORE: OK"

# ---------------------------------------------------------------------------
# R1-6 step 2: recreate db on the new init scripts, rerun init-users.sh, roles.
# ---------------------------------------------------------------------------
echo "== R1-6 step 2: new roles"
compose_new up -d --no-deps --force-recreate --wait db > "$work/logs/recreate-db.log" 2>&1 || { tail -n 20 "$work/logs/recreate-db.log" >&2; fail "db did not come back on the new init scripts"; }
compose_new exec -T db grep -q baby_tracker_live_activity /docker-entrypoint-initdb.d/10-init-roles.sql || fail "recreated db does not mount the new init-roles.sql"
init_users_rerun step2 compose_new || fail "init-users.sh rerun failed (R1-6 step 2)"
roles=$(db_psql -At -F ' ' -c "SELECT rolname, rolcanlogin, rolpassword IS NULL FROM pg_authid WHERE rolname IN ('baby_tracker_live_activity','baby_tracker_live_activity_app') ORDER BY rolname" | tr '\n' ';')
[[ $roles == 'baby_tracker_live_activity f t;baby_tracker_live_activity_app t t;' ]] || fail "roles after the rerun are '$roles' (R1-6 step 2)"
[[ $(schema) == 27 ]] || fail "schema changed during the role step"
echo "ROLES: OK (worker login passwordless)"

# ---------------------------------------------------------------------------
# R1-6 step 3: migrate; only 28 applies.
# ---------------------------------------------------------------------------
echo "== R1-6 step 3: migrate"
[[ $(schema) == 27 ]] || fail "schema is not 27 before migrate"
compose_new run --rm --no-deps migrate > "$work/logs/migrate.log" 2>&1 || { tail -n 5 "$work/logs/migrate.log" >&2; fail "migrate failed"; }
[[ $(schema) == 29 ]] || fail "schema after migrate is $(schema), not 29"
[[ $(db_psql -At -c 'SELECT count(*)=current_version AND min(version)=1 AND max(version)=current_version FROM schema_migrations CROSS JOIN schema_compatibility WHERE singleton GROUP BY current_version') == t ]] || fail "migrations are not contiguous after 28"
[[ $(db_psql -At -c 'SELECT sha256 FROM schema_migrations WHERE version = 28') == "$digest_28" ]] || fail "digest of 28 differs from the new tree's manifest"
[[ $(db_psql -At -c 'SELECT max(schema_version) FROM schema_privilege_manifest') == 29 ]] || fail "privilege manifest is not 29"
echo "SCHEMA 28: OK (digest $digest_28)"

# ---------------------------------------------------------------------------
# Content and inventories: every pre-existing table unchanged except the three
# the migration writes; new tables empty; privileges exactly the manifest's.
# ---------------------------------------------------------------------------
echo "== Content and inventories after 28"
inventory > "$work/inventory.28"
compare_inventories "$work/inventory.27" "$work/inventory.28" "public.schema_migrations public.schema_compatibility public.schema_privilege_manifest" || fail "rows changed across migration 28"
new_tables=$(comm -13 <(cut -d' ' -f1 "$work/inventory.27") <(cut -d' ' -f1 "$work/inventory.28") | tr '\n' ' ')
for table in $new_tables; do [[ $table == public.live_activity_* ]] || fail "unexpected new table $table"; done
[[ $(cut -d' ' -f1 "$work/inventory.28" | grep -c '^public.live_activity_') -ge 5 ]] || fail "migration 28 created fewer live_activity tables than expected"
echo "rows: $(( $(wc -l < "$work/inventory.27") )) tables unchanged in count and digest; new empty tables: $new_tables"
# The bootstrap rerun on 28 raises 'effective privileges do not match schema
# privilege manifest' unless every principal's effective privileges equal the
# manifest's rows for 28 exactly.
init_users_rerun after-28 compose_new || fail "init-users.sh rerun on 28 failed: the effective privileges differ from the manifest"
docker exec -e BT_PRINCIPAL_NEGATIVE_PROBES=0 -i "$(db_container)" sh -s < "$new_tree/Server/scripts/check-principals.sh" > "$work/logs/check-principals.log" 2>&1 \
  || { tail -n 3 "$work/logs/check-principals.log" >&2; fail "check-principals.sh failed"; }
grep -q 'principal proof: positive role and exact current privilege contract verified' "$work/logs/check-principals.log" || fail "check-principals.sh printed no proof"
role_inventory() {
  db_psql -At -c "SELECT string_agg(rolname||':'||rolcanlogin::text||':'||(rolpassword IS NOT NULL)::text, ' ' ORDER BY rolname) FROM pg_authid WHERE rolname NOT LIKE 'pg\\_%' AND rolname <> 'postgres'"
}
# role:can-login:has-password. Only the worker login is passwordless after R1
# (no live_activity_password mounted); the self-hosted revoker login too.
expected_roles='baby_tracker_app:true:true baby_tracker_apple_revoker:false:false baby_tracker_apple_revoker_app:true:false baby_tracker_export_app:true:true baby_tracker_live_activity:false:false baby_tracker_live_activity_app:true:false baby_tracker_maintenance:false:false baby_tracker_migrator:true:true baby_tracker_projector:false:false baby_tracker_projector_app:true:true baby_tracker_runtime:false:false'
observed_roles=$(role_inventory)
[[ $observed_roles == "$expected_roles" ]] || fail "role inventory is not exact: $observed_roles"
[[ $(db_psql -At -c "SELECT count(*) FROM pg_authid WHERE rolname NOT LIKE 'pg\\_%' AND rolname <> 'postgres' AND (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)") == 0 ]] || fail "an application role has an elevated attribute"
memberships=$(db_psql -At -c "SELECT string_agg(parent.rolname||'<-'||member.rolname, ' ' ORDER BY parent.rolname) FROM pg_auth_members m JOIN pg_roles parent ON parent.oid=m.roleid JOIN pg_roles member ON member.oid=m.member WHERE parent.rolname NOT LIKE 'pg\\_%'")
[[ $memberships == 'baby_tracker_apple_revoker<-baby_tracker_apple_revoker_app baby_tracker_live_activity<-baby_tracker_live_activity_app baby_tracker_maintenance<-baby_tracker_export_app baby_tracker_projector<-baby_tracker_projector_app baby_tracker_runtime<-baby_tracker_app' ]] || fail "memberships are not exact: $memberships"
echo "INVENTORIES: OK (11 roles, 5 memberships, effective privileges = manifest 28)"

# ---------------------------------------------------------------------------
# The old image refuses schema 28: its migrate, and its server never ready.
# ---------------------------------------------------------------------------
echo "== Old image against schema 28"
if compose_old run --rm --no-deps migrate > "$work/logs/old-migrate.log" 2>&1; then
  fail "the old image's migrate accepted schema 28"
fi
grep -q 'database_newer_than_binary: version 28' "$work/logs/old-migrate.log" || { tail -n 3 "$work/logs/old-migrate.log" >&2; fail "the old image's migrate failed for another reason"; }
compose_old up -d --no-deps app > "$work/logs/old-app.log" 2>&1 || { tail -n 10 "$work/logs/old-app.log" >&2; fail "old app container did not start"; }
base=$(app_base)
healthy=0
for _ in $(seq 1 60); do
  [[ $(request "$base" /healthz) == 200 ]] && { healthy=1; break; }
  sleep 0.5
done
[[ $healthy -eq 1 ]] || fail "old app served no /healthz"
for _ in $(seq 1 20); do
  status=$(request "$base" /readyz)
  [[ $status == 503 ]] || fail "old app answered /readyz $status on schema 28; expected 503 throughout"
  sleep 0.25
done
jq -e '.error.code == "not_ready"' "$work/response.json" >/dev/null || fail "old app's /readyz body is not not_ready"
echo "OLD IMAGE: refused (migrate: database_newer_than_binary: version 28; server /healthz 200, /readyz 503 for 5 s)"

# ---------------------------------------------------------------------------
# R1-6 step 4: the new image with push off.
# ---------------------------------------------------------------------------
echo "== R1-6 step 4: new app, push off"
[[ $(schema) == 29 ]] || fail "schema is not 29 before the app start"
compose_new up -d --no-deps --force-recreate --wait --wait-timeout 120 app > "$work/logs/new-app.log" 2>&1 || { tail -n 10 "$work/logs/new-app.log" >&2; fail "new app did not start"; }
base=$(app_base)
wait_ready "$base" 200 180 || {
  # The reason, as the operator would read it: security verify's result word.
  verification=$(compose_new run --rm --no-deps migrate security verify 2>&1 | tail -n 1 | sed 's/[("].*//')
  fail "new app never became ready; security verify: ${verification:-no output} (ledger, actor-ID key, identity or schema mismatch)"
}
[[ $(request "$base" /v1/server) == 200 ]] || fail "/v1/server failed"
jq -e --arg instance "$instance_id" --argjson epoch "$epoch" --arg source "$new_revision" \
  '.instance_id == $instance and .recovery_epoch == $epoch and .source_version == $source and (.capabilities | index("live-activity-push-v1") == null)' \
  "$work/response.json" >/dev/null || { jq -c '{source_version, instance_id, recovery_epoch, capabilities}' "$work/response.json" >&2; fail "/v1/server identity or capabilities differ (R1-7)"; }
[[ $(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$(compose_new ps -q app)" | grep -c '^BT_LIVE_ACTIVITY_PUSH_ENABLED=true$') == 0 ]] || fail "push is enabled on the app"
echo "NEW IMAGE: ready (readyz 200, instance $instance_id, epoch $epoch, source $new_revision, no live-activity-push-v1)"

# ---------------------------------------------------------------------------
# R2's database half: mount a worker password, rerun init-users.sh, then the
# preflight's database checks (no Apple contact: --skip-apple, a disposable
# key that is nobody's credential).
# ---------------------------------------------------------------------------
echo "== R2 database half: worker password and preflight --skip-apple"
secret > "$secrets/live_activity_password"
printf 'postgres://baby_tracker_live_activity_app:%s@db:5432/babytracker?sslmode=disable\n' "$(<"$secrets/live_activity_password")" > "$secrets/live_activity_database_url"
openssl ecparam -name prime256v1 -genkey -noout 2>/dev/null | openssl pkcs8 -topk8 -nocrypt -out "$secrets/apns_key.p8"
chmod 0444 "$secrets/live_activity_password"
chmod 0600 "$secrets/live_activity_database_url" "$secrets/apns_key.p8"
if [[ $(id -u) -eq 0 ]]; then chown 65532:65532 "$secrets/live_activity_database_url" "$secrets/apns_key.p8"; chown 70:70 "$secrets/live_activity_password"; fi
compose_new stop app > /dev/null 2>&1
compose_push up -d --no-deps --wait --wait-timeout 120 db > "$work/logs/push-db.log" 2>&1 || { tail -n 20 "$work/logs/push-db.log" >&2; fail "db did not come back with the worker password mounted"; }
compose_push exec -T db test -s /run/secrets/live_activity_password || fail "db does not mount live_activity_password"
init_users_rerun r2 compose_push || fail "init-users.sh rerun with the worker password failed"
[[ $(db_psql -At -c "SELECT rolpassword IS NOT NULL FROM pg_authid WHERE rolname = 'baby_tracker_live_activity_app'") == t ]] || fail "worker login has no password after the rerun"
compose_push run --rm --no-deps migrate > "$work/logs/migrate-r2.log" 2>&1 || fail "migrate rerun failed"
[[ $(schema) == 29 ]] || fail "schema is not 29 after the R2 rerun"
if ! compose_push run --rm --no-deps app live-activity-preflight --environment sandbox --skip-apple > "$work/logs/preflight.log" 2>&1; then
  grep -E '^\[(FAIL|UNKNOWN|RETRY)\]|^RESULT' "$work/logs/preflight.log" >&2
  fail "live-activity-preflight --skip-apple failed"
fi
grep -qF '[PASS] worker database: ' "$work/logs/preflight.log" || fail "preflight did not pass the worker database check"
grep -q '^RESULT: PASS' "$work/logs/preflight.log" || fail "preflight did not print RESULT: PASS"
echo "PREFLIGHT: $(grep -F '[PASS] worker database: ' "$work/logs/preflight.log")"
# Back to push off (X-1): the app without the overlay, ready again.
compose_new up -d --no-deps --wait --wait-timeout 120 app > "$work/logs/new-app-2.log" 2>&1 || fail "app did not restart with push off"
base=$(app_base)
wait_ready "$base" 200 180 || fail "app not ready after the worker password step"
[[ $(request "$base" /v1/server) == 200 ]] && jq -e '.capabilities | index("live-activity-push-v1") == null' "$work/response.json" >/dev/null || fail "capability advertised with push off"

# ---------------------------------------------------------------------------
# Final: every care table as before 28; roles exact with the worker password.
# ---------------------------------------------------------------------------
echo "== Final content check"
inventory > "$work/inventory.final"
# Only what migration 28 writes, plus server_state: the recovery gate's
# Reconcile rewrites the database mirror (recovery/gate.go, mirror.Update)
# every time an app starts, so its row text changes while its identity does
# not; the identity itself was checked through /v1/server above.
operational="public.schema_migrations public.schema_compatibility public.schema_privilege_manifest public.server_state"
compare_inventories "$work/inventory.27" "$work/inventory.final" "$operational" || fail "care rows changed during the rehearsal"
observed_roles=$(role_inventory)
[[ $observed_roles == "${expected_roles/baby_tracker_live_activity_app:true:false/baby_tracker_live_activity_app:true:true}" ]] || fail "final role inventory is not exact: $observed_roles"
[[ $(schema) == 29 ]] || fail "final schema is not 29"
echo "final: $(care_counts)"
echo "REHEARSAL: PASS (schema 27 -> 28, roles and privileges exact, old image refused, new image ready with push off, preflight database checks passed, care rows unchanged)"
