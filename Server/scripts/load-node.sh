#!/usr/bin/env bash
# Phase 5 load runner for the declared reference node: app and PostgreSQL
# each capped at 4 CPU / 8 GiB, the generator uncapped beside them
# (Server/deploy/load/compose.yaml), on the Mac's OrbStack or on a Linux VM's
# Docker Engine (`uname -s`). The results carry the node's label, computed
# from the host facts ("Apple M4, Docker CPU caps"; "AMD Ryzen 7 7800X3D
# (Hyper-V VM, 12 vCPU, 16 GiB), Docker CPU caps") unless BT_LOAD_NODE_LABEL
# names it.
#
#   load-node.sh preflight --families N            # idle check, lock, disk; writes $RUN/idle.json
#   load-node.sh up        --run RUN               # secrets, compose up db, migrate, authority init, caps verify, pg_stat_statements
#   load-node.sh verify-caps --run RUN             # docker inspect + cgroup files + burn bound → $RUN/caps.json; refuses on mismatch
#   load-node.sh seed      --run RUN --families N --seed S [--backlog-load L] [--as-of RFC3339] [--history r10k|small] [--workers W]
#                          [--hot-family]          # one more family, 5 accounts x 2 devices: the hot-family scenario's
#                          the seed alone runs with wal_buffers BT_LOAD_SEED_WAL_BUFFERS (256MB), reset after it
#   load-node.sh start     --run RUN               # app up, readiness, runtime-limits log check
#   load-node.sh run       --run RUN --scenario soak|ramp|storm|restart|mass-snapshot|hot-family|flood|import|export \
#                          --load L --warmup D --minutes M [--profile-at 5m,15m,25m|steps] [--thresholds FILE] [--seed S]
#                          [--<scenario option> VALUE …]   # passed to the generator as given (--steps, --storm-at, --model-override live-only, …)
#                          --profile-at steps (ramp): one profile in each --steps step, its CPU profile ending 15 s
#                          before the step does (combinable with offsets: steps,1m)
#                          --maintenance hot [--maintenance-at D]: one family purge and one export inside a soak
#                          restart: at window-start + --restart-at (Ns|Nm|Nh) the app is stopped (-t 30) and started
#                          again; the stop, start and ready times go to $RUN/restart.json
#   load-node.sh profile   --run RUN --label X     # cpu 30 s, heap, mutex, block, goroutine → $RUN/profiles/X-*
#   load-node.sh audit     --run RUN
#   load-node.sh upgrade   --run RUN [--recreate-db]  # between measurements: build HEAD, stop the app, migrate the seeded
#                          database (schema before/after and duration), start the app as start does; upgrade.json, and
#                          upgrades.jsonl for the run's history. The db container is kept, so a changed db command line
#                          in the overlay applies only with --recreate-db (CHECKPOINT, recreate, 120 s stop, healthy wait)
#   load-node.sh down      --run RUN [--keep-volume]
#
# RUN is an absolute directory /private/tmp/bt-load/<run-id> on the Mac,
# /tmp/bt-load/<run-id> on Linux; BT_LOAD_RUN may name it instead of --run.
# Only BT_LOAD_* variables may be inherited (the tunables the overlay reads:
# BT_LOAD_SOCKET_LIMIT, BT_LOAD_RUNTIME_CONNS, BT_LOAD_MAINTENANCE_CONNS,
# BT_LOAD_ADMISSION, BT_LOAD_ADMISSION_WAIT, BT_LOAD_APNS_LATENCY_MS,
# BT_LOAD_LOG_AUTOVACUUM_MIN_DURATION (the db's command line), BT_LOAD_DB_PORT,
# BT_LOAD_PPROF_PORT; and the runner's own: BT_LOAD_PROJECT, BT_LOAD_LOCK,
# BT_LOAD_LOCK_HOURS, BT_LOAD_IDLE_*, BT_LOAD_BURN_*, BT_LOAD_*_INTERVAL,
# BT_LOAD_CPU_WINDOW, BT_LOAD_NODE_LABEL).
#
# The node may be shared with other sessions. preflight takes the lock in
# /private/tmp/bt-load.lock (/tmp/bt-load.lock on Linux) that every other run
# respects, and every later subcommand requires it; down releases it. Nothing
# here restarts OrbStack or Docker, changes their configuration, or touches a
# container outside this run's Compose project.
set -euo pipefail
umask 077

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
server_dir=$(cd "$script_dir/.." && pwd)
repo_root=$(cd "$server_dir/.." && pwd)

# Option discipline (as sync-performance.sh): no inherited application or
# connection setting can silently change the composition.
for name in $(compgen -v BT_); do
  case $name in
    BT_LOAD_*) ;;
    *) echo "load-node: refusing inherited $name: the runner sets every BT_* value itself (only BT_LOAD_* tunables pass)" >&2; exit 2 ;;
  esac
done

# shellcheck source=lib/load_host.sh
source "$script_dir/lib/load_host.sh"
# shellcheck source=lib/load_samplers.sh
source "$script_dir/lib/load_samplers.sh"

case $bt_load_os in
  Darwin | Linux) ;;
  *) echo "load-node: unsupported host '$bt_load_os' (uname -s): the runner knows Darwin and Linux" >&2; exit 2 ;;
esac
readonly load_root=$bt_load_tmp/bt-load
readonly instance_id=70000000-0000-4000-8000-0000000a0d01
readonly pg_image=postgres:17.6-alpine@sha256:ef257d85f76e48da1c64832459b59fcaba1a4dac97bf5d7450c77753542eee94
readonly capped="4000000000 8589934592 8589934592"
readonly uncapped="0 0 0"
readonly app_base=http://172.31.250.10:8080
readonly app_host=sync.load.test
readonly generator_ip=172.31.250.100
readonly load_stack_marker=baby-tracker-load-stack
project=$BT_LOAD_PROJECT

die() { echo "load-node: $*" >&2; exit 2; }
fail() { echo "load-node: $*" >&2; exit 1; }
now_iso() { date -u +%Y-%m-%dT%H:%M:%SZ; }

[[ $project =~ ^[a-z0-9][a-z0-9_-]{0,40}$ ]] || die "invalid BT_LOAD_PROJECT"
# A bad label override is refused before anything runs.
if [[ -n ${BT_LOAD_NODE_LABEL:-} ]]; then bt_load_node_label >/dev/null || exit 2; fi

# node_label: the label every artifact carries (caps.json, measurement.json,
# resources.json, verdict.json); set by resolve_node_label [FACTS].
node_label=""
resolve_node_label() {
  node_label=$(bt_load_node_label "${1:-}") || die "could not name the node"
  [[ -n $node_label ]] || die "could not name the node"
}

# The user the app and the generator run as (the overlay's user:). On the
# Mac it is the images' own 65532 (OrbStack's file sharing lets it read the
# run's 0600/0700 files). Docker Engine on Linux mounts the run directory with
# its real owner, so there they run as the invoking user, who owns the
# secrets, the keyrings and the run directory the generator writes to (it
# refuses a fleet directory another user owns).
if [[ $bt_load_os == Linux ]]; then
  container_user=$(id -u):$(id -g)
else
  container_user=65532:65532
fi
[[ $container_user =~ ^[0-9]+:[0-9]+$ ]] || die "could not read this user's uid and gid"

usage() {
  awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}" >&2
  exit 2
}

# --- cleanup ---------------------------------------------------------------------

burn_started=0
seed_wal_applied=0
sampling=0
sampler_pids=""
generator_pid=""
release_on_failure=0
upgrade_record="" upgrade_pending=0 upgrade_stage=""
RUN=""

on_exit() {
  local status=$?
  trap - EXIT
  set +e
  if [[ -n $generator_pid ]]; then stop_generator; fi
  if [[ -n $RUN ]]; then rm -f "$RUN/.seeding"; fi
  if ((seed_wal_applied)); then
    # An interrupted seed's container may still be restoring the schema it
    # deferred: the db restart waits for it (at most BT_LOAD_SEED_RESTORE_WAIT).
    seed_wait_for_seeder
    seed_wal_reset || echo "load-node: could not reset wal_buffers after the seed; run: load-node.sh down, then a new stack" >&2
  fi
  if ((burn_started)); then docker rm -f "$project-burn" >/dev/null 2>&1; fi
  if ((sampling)); then stop_samplers; fi
  if ((upgrade_pending)); then upgrade_write failed "the upgrade stopped at its $upgrade_stage stage (exit $status); see the output above"; fi
  if ((release_on_failure && status != 0)); then bt_load_release_lock "$RUN"; fi
  exit "$status"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# loadgen_containers: running generator containers of this project (the
# `compose run` one-offs included), by label only.
loadgen_containers() {
  docker ps -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=loadgen" 2>/dev/null || true
}

# stop_generator: the Compose client first, then the container itself. A
# TERM to the backgrounded client does not reach the container, which would
# otherwise go on generating load into $RUN after the runner has gone.
stop_generator() {
  local id
  if [[ -n $generator_pid ]]; then
    kill -TERM "$generator_pid" 2>/dev/null || true
    wait "$generator_pid" 2>/dev/null || true
    generator_pid=""
  fi
  for id in $(loadgen_containers); do
    docker stop "$id" >/dev/null 2>&1 || true
  done
}

# lease_renewer SENTINEL: renew the lock's lease every BT_LOAD_LOCK_RENEW
# (300) seconds while SENTINEL exists and the runner ($$, the main shell's pid
# in every subshell) is alive. A SIGKILLed runner runs no cleanup; without the
# pid check this loop would keep the lock alive forever. Every background
# loop (samplers, profiler) carries the same check.
lease_renewer() {
  local sentinel=$1 every=${BT_LOAD_LOCK_RENEW:-300} n=0
  if [[ $every =~ ^[0-9]{1,5}$ ]]; then every=$((10#$every)); else every=300; fi
  if ((every < 1)); then every=300; fi
  while [[ -e $sentinel ]] && kill -0 "$$" 2>/dev/null; do
    sleep 1
    n=$((n + 1))
    if ((n >= every)); then
      if [[ ! -e $sentinel ]] || ! kill -0 "$$" 2>/dev/null; then break; fi
      n=0
      bt_load_renew_lock "$RUN" || echo "load-node: could not renew the lock lease" >&2
    fi
  done
}

# --- options -----------------------------------------------------------------------

opt_run="" opt_families="" opt_seed="" opt_backlog_load="" opt_as_of="" opt_history="" opt_workers=""
opt_scenario="" opt_load="" opt_warmup="" opt_minutes="" opt_profile_at="" opt_thresholds="" opt_label=""
opt_keep_volume=0 opt_hot_family=0 opt_recreate_db=0
passthrough=()

parse_options() {
  local allowed=$1 key
  shift
  while (($#)); do
    [[ $1 == --* ]] || die "unexpected argument $1"
    key=${1#--}
    if [[ $key == keep-volume && " $allowed " == *" keep-volume "* ]]; then opt_keep_volume=1; shift; continue; fi
    if [[ $key == hot-family && " $allowed " == *" hot-family "* ]]; then opt_hot_family=1; shift; continue; fi
    if [[ $key == recreate-db && " $allowed " == *" recreate-db "* ]]; then opt_recreate_db=1; shift; continue; fi
    (($# >= 2)) || die "missing value for $1"
    if [[ " $allowed " != *" $key "* ]]; then
      # `run` hands scenario options it does not know (--steps, --storm-at,
      # --restart-at, …) to the generator, which validates them.
      [[ " $allowed " == *" passthrough "* && $key =~ ^[a-z][a-z-]*$ && $2 =~ ^[A-Za-z0-9.,:_+-]+$ ]] || die "unknown option $1"
      passthrough+=("$1" "$2")
      shift 2
      continue
    fi
    case $key in
      run) opt_run=$2 ;;
      families) opt_families=$2 ;;
      seed) opt_seed=$2 ;;
      backlog-load) opt_backlog_load=$2 ;;
      as-of) opt_as_of=$2 ;;
      history) opt_history=$2 ;;
      workers) opt_workers=$2 ;;
      scenario) opt_scenario=$2 ;;
      load) opt_load=$2 ;;
      warmup) opt_warmup=$2 ;;
      minutes) opt_minutes=$2 ;;
      profile-at) opt_profile_at=$2 ;;
      thresholds) opt_thresholds=$2 ;;
      label) opt_label=$2 ;;
    esac
    shift 2
  done
}

# duration_seconds 10m → 600 (Ns, Nm or Nh), read in base 10 ("08m" is 8
# minutes, not a bad octal number).
duration_seconds() {
  [[ $1 =~ ^([0-9]{1,6})(s|m|h)$ ]] || die "invalid duration $1 (use Ns, Nm or Nh)"
  case ${BASH_REMATCH[2]} in
    s) echo $((10#${BASH_REMATCH[1]})) ;;
    m) echo $((10#${BASH_REMATCH[1]} * 60)) ;;
    h) echo $((10#${BASH_REMATCH[1]} * 3600)) ;;
  esac
}

# whole_number VALUE NAME: a base-10 integer, printed canonically ("08" → 8).
whole_number() {
  [[ $1 =~ ^[0-9]{1,9}$ ]] || die "invalid $2: $1"
  echo $((10#$1))
}

# decimal_number VALUE NAME: a non-negative decimal, printed canonically
# ("01" → 1, "0.50" → 0.5) so it is valid JSON and a valid Go float.
decimal_number() {
  [[ $1 =~ ^[0-9]{1,6}(\.[0-9]{1,6})?$ ]] || die "invalid $2: $1"
  awk -v x="$1" 'BEGIN { printf "%.6g\n", x + 0 }'
}

# normalize_options: every number is validated and made canonical before
# anything runs, so nothing fails on it after a measurement.
normalize_options() {
  [[ -z $opt_families ]] || opt_families=$(whole_number "$opt_families" --families)
  [[ -z $opt_seed ]] || opt_seed=$(whole_number "$opt_seed" --seed)
  [[ -z $opt_minutes ]] || opt_minutes=$(whole_number "$opt_minutes" --minutes)
  [[ -z $opt_workers ]] || opt_workers=$(whole_number "$opt_workers" --workers)
  [[ -z $opt_load ]] || opt_load=$(decimal_number "$opt_load" --load)
  [[ -z $opt_backlog_load ]] || opt_backlog_load=$(decimal_number "$opt_backlog_load" --backlog-load)
  return 0
}

# --- the run directory -----------------------------------------------------------

# resolve_run CREATE: RUN must be one directory directly under
# /private/tmp/bt-load, never a symlink, owned by this user.
resolve_run() {
  local create=$1 name
  [[ -n $RUN ]] || die "name the run with --run or BT_LOAD_RUN"
  name=${RUN#"$load_root"/}
  if [[ $RUN != "$load_root"/* || -z $name || $name == */* || $name == . || $name == .. || ! $name =~ ^[A-Za-z0-9._-]+$ ]]; then
    die "runs live only under $load_root/ (one directory per run), not $RUN"
  fi
  [[ ! -L $load_root ]] || die "symlinked output refused: $load_root"
  [[ ! -L $RUN ]] || die "symlinked output refused: $RUN"
  if [[ ! -e $RUN ]]; then
    ((create)) || die "no such run: $RUN"
    mkdir -p "$load_root"
    mkdir -m 0700 "$RUN"
  fi
  [[ -d $RUN && -O $RUN ]] || die "the run is not a directory owned by this user: $RUN"
}

# guard_out NAME…: refuse to write through a symlink.
guard_out() {
  local name
  for name in "$@"; do
    [[ ! -L $RUN/$name ]] || die "symlinked output refused: $RUN/$name"
  done
}

# load_state: the run's secrets directory and source SHA, written by up.
secrets="" source_sha=""
load_state() {
  guard_out private private/secrets source-sha
  secrets=$RUN/private/secrets
  [[ -d $secrets && -f $RUN/source-sha ]] || fail "the run is not up (no private/secrets): $RUN"
  source_sha=$(<"$RUN/source-sha")
}

# compose …: this run's project, always with both files, an empty env file
# (no implicit .env) and every BT_* value the base file interpolates.
compose() {
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES -u COMPOSE_ENV_FILES -u COMPOSE_DISABLE_ENV_FILE \
    BT_SECRETS_DIR="$secrets" BT_KEYRING_DIR="$RUN/private/keyrings" BT_MCP_SECRETS_DIR="$RUN/private/mcp" \
    BT_LOAD_RUN="$RUN" BT_LOAD_PROJECT="$project" BT_SOURCE_SHA="$source_sha" BT_IMAGE_NAME="$project-server:identitytest" \
    BT_SERVER_INSTANCE_ID="$instance_id" BT_RECOVERY_EPOCH=1 BT_APPLE_AUDIENCE=com.example.babytracker.load \
    BT_PUBLIC_ORIGIN="https://$app_host" BT_HTTP_PORT=0 BT_LOAD_CONTAINER_USER="$container_user" \
    docker compose --env-file "$RUN/private/compose.env" --project-name "$project" --project-directory "$server_dir" \
    -f "$server_dir/compose.yaml" -f "$server_dir/deploy/load/compose.yaml" "$@"
}

container_of() { compose ps -aq "$1" 2>/dev/null | awk 'NR == 1'; }

# up builds the generator image when Server/cmd/baby-tracker-load exists
# (Tasks 4, 10 and 11 add it).
require_generator() {
  docker image inspect "$project-loadgen:identitytest" >/dev/null 2>&1 ||
    fail "the generator image $project-loadgen:identitytest is not built (Server/cmd/baby-tracker-load comes with Tasks 4, 10 and 11; up builds it)"
}

require_app_running() {
  local app
  app=$(container_of app)
  [[ -n $app && $(docker inspect -f '{{.State.Running}}' "$app" 2>/dev/null) == true ]] || fail "the app is not running; run start first"
}

tunables_json() {
  local name pairs=""
  for name in $(compgen -e | awk '/^BT_LOAD_/'); do pairs="$pairs$name=${!name}"$'\n'; done
  jq -cn --arg pairs "$pairs" '$pairs | split("\n") | map(select(length > 0) | capture("^(?<key>[^=]+)=(?<value>.*)$")) | from_entries'
}

# flood_evictions RUN: the flood cell's check (Task 19). Each IP scope's
# limiter evictions over the run (the last capacity sample's count less the
# first's in app.log; the flood runs only in the measurement) go to
# result.json cells.flood.ip_evictions, and evictions_ok says whether every
# scope the cell names in evictions_expected evicted: a flood that never
# filled a pool measured nothing. A failed check is warned, not fatal.
flood_evictions() {
  local result=$1/result.json evictions missing
  [[ -f $result ]] && jq -e '.cells.flood' "$result" >/dev/null 2>&1 || return 0
  evictions=$(jq -cR 'fromjson? | select(.msg == "capacity resource sample") | .sample.limiter.ip_evictions // empty' "$1/app.log" 2>/dev/null |
    jq -cs 'if length == 0 then {} else (.[0]) as $first | .[-1] | with_entries(.value -= ($first[.key] // 0)) end') || evictions='{}'
  jq --argjson ev "$evictions" '
    .cells.flood.ip_evictions = $ev
    | ([.cells.flood.evictions_expected // [] | .[] | select(($ev[.] // 0) == 0)] | join(",")) as $missing
    | .cells.flood.evictions_ok = ($missing == "")
    | if $missing == "" then . else .cells.flood.evictions_missing = $missing end' "$result" > "$result.tmp" && mv "$result.tmp" "$result"
  missing=$(jq -r '.cells.flood.evictions_missing // ""' "$result")
  [[ -z $missing ]] || echo "load-node: flood cell invalid: no IP evictions in $missing (the flood did not fill those pools)" >&2
}

# applied_tunables_json APP: the overlay's app tunables as the running app
# applied them (its container environment), under their BT_LOAD_* names. A
# run's own environment need not name them: start applied them, perhaps in
# another shell. Fails when there is no container or it cannot be inspected.
applied_tunables_json() {
  local env
  [[ -n $1 ]] || return 1
  env=$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$1" 2>/dev/null) || return 1
  jq -cn --arg env "$env" '
    {BT_SOCKET_PROCESS_LIMIT: "BT_LOAD_SOCKET_LIMIT", BT_DB_RUNTIME_MAX_CONNS: "BT_LOAD_RUNTIME_CONNS",
     BT_DB_MAINTENANCE_MAX_CONNS: "BT_LOAD_MAINTENANCE_CONNS", BT_ADMISSION_TOTAL: "BT_LOAD_ADMISSION",
     BT_IDENTITYTEST_APNS_SINK_LATENCY_MS: "BT_LOAD_APNS_LATENCY_MS", BT_ADMISSION_WAIT: "BT_LOAD_ADMISSION_WAIT"} as $names
    | $env | split("\n") | map(capture("^(?<key>[A-Z0-9_]+)=(?<value>.*)$") // empty
      | select($names[.key]) | {key: $names[.key], value}) | from_entries'
}

# applied_db_tunables_json DB: the overlay's db tunables as the running db
# was started with them (its container command line), under their BT_LOAD_*
# names. Fails when there is no container or it cannot be inspected.
applied_db_tunables_json() {
  local cmd
  [[ -n $1 ]] || return 1
  cmd=$(docker inspect -f '{{range .Config.Cmd}}{{println .}}{{end}}' "$1" 2>/dev/null) || return 1
  jq -cn --arg cmd "$cmd" '
    {log_autovacuum_min_duration: "BT_LOAD_LOG_AUTOVACUUM_MIN_DURATION"} as $names
    | $cmd | split("\n") | map(capture("^(?<key>[a-z_.]+)=(?<value>.*)$") // empty
      | select($names[.key]) | {key: $names[.key], value}) | from_entries'
}

# db_tuning_json: the db's tuning settings an A/B changes on its command line
# (the background writer, the autovacuum log threshold), as the db reports
# them.
db_tuning_json() {
  jq -cn --arg autovacuum_log "$(db_setting log_autovacuum_min_duration)" --arg lru "$(db_setting bgwriter_lru_maxpages)" \
    --arg delay "$(db_setting bgwriter_delay)" \
    '{log_autovacuum_min_duration: $autovacuum_log, bgwriter_lru_maxpages: $lru, bgwriter_delay: $delay}'
}

# --- preflight -----------------------------------------------------------------

cmd_preflight() {
  [[ $opt_families =~ ^[0-9]+$ && $opt_families -ge 1 ]] || die "--families N is required"
  resolve_run 1
  guard_out idle.json .idle-samples.jsonl
  local held_before=0 samples=$RUN/.idle-samples.jsonl idle=true reason="" disk="" disk_ok=true existing
  [[ -d $BT_LOAD_LOCK && $(bt_load_lock_field run) == "$RUN" ]] && held_before=1
  bt_load_take_lock "$RUN" || exit 1
  ((held_before)) || release_on_failure=1
  if ((!held_before)); then
    existing=$(docker ps -aq --filter "label=com.docker.compose.project=$project" 2>/dev/null || true)
    [[ -z $existing ]] || fail "a previous $project stack still exists; end it first with: load-node.sh down --run <its run>"
  fi
  bt_load_idle_check "$samples" || idle=false
  reason=$(jq -rs 'map(.reason // "") | map(select(length > 0)) | last // ""' "$samples" 2>/dev/null || true)
  # A refusal with no recorded reason is a sample that could not be taken.
  if [[ $idle == false && -z $reason ]]; then reason="could not sample the host"; fi
  disk=$(bt_load_disk_check "$opt_families") || disk_ok=false
  jq -n --slurpfile samples "$samples" --arg checked_at "$(now_iso)" --argjson families "$opt_families" \
    --argjson idle "$idle" --argjson disk_ok "$disk_ok" --arg reason "$reason" --arg disk "$disk" \
    --arg others "$bt_load_others" --arg lock "$BT_LOAD_LOCK" --arg project "$project" '
    ($disk | split(" ")) as $d
    | {checked_at: $checked_at, idle: ($idle and $disk_ok), families: $families, reason: $reason,
       samples: $samples, other_containers: ($others | split("\n") | map(select(length > 0))),
       disk: {avail_gb: (($d[0] // "0") | tonumber), need_gb: (($d[1] // "0") | tonumber), ok: $disk_ok},
       lock: $lock, project: $project}' > "$RUN/idle.json"
  rm -f "$samples"
  [[ $idle == true && $disk_ok == true ]] || exit 1
  release_on_failure=0
  echo "preflight passed: $RUN holds $BT_LOAD_LOCK until down (lease ${BT_LOAD_LOCK_HOURS:-4} h, renewed by every subcommand)"
}

# --- up ------------------------------------------------------------------------

write_secrets() {
  local secret
  mkdir -m 0700 "$RUN/private" "$RUN/private/secrets" "$RUN/private/keyrings"
  # The MCP cursor key directory is mounted read-only at /run/mcp, as in
  # mcp-e2e.sh.
  mkdir -m 0755 "$RUN/private/mcp"
  secrets=$RUN/private/secrets
  for secret in bootstrap_password migrator_password runtime_password export_password identity_pepper session_token_pepper \
    device_continuity_pepper apple_notification_pepper invitation_pepper page_token_key actor_id_pepper rate_limit_pepper; do
    openssl rand -hex 32 > "$secrets/$secret"
  done
  printf 'postgres://baby_tracker_migrator:%s@db:5432/babytracker?sslmode=disable\n' "$(<"$secrets/migrator_password")" > "$secrets/migrator_database_url"
  printf 'postgres://baby_tracker_app:%s@db:5432/babytracker?sslmode=disable\n' "$(<"$secrets/runtime_password")" > "$secrets/runtime_database_url"
  printf 'postgres://baby_tracker_export_app:%s@db:5432/babytracker?sslmode=disable\n' "$(<"$secrets/export_password")" > "$secrets/maintenance_database_url"
  # The seeder and the post-run audit connect as the bootstrap superuser.
  printf 'postgres://postgres:%s@db:5432/babytracker?sslmode=disable\n' "$(<"$secrets/bootstrap_password")" > "$secrets/superuser_database_url"
  chmod 0444 "$secrets"/*_password "$secrets"/*_pepper "$secrets"/*_url "$secrets/page_token_key"
  # A throwaway P-256 APNs key: the identitytest sink records every push, and
  # nothing can reach Apple. The server requires 0600.
  openssl ecparam -name prime256v1 -genkey -noout 2>/dev/null | openssl pkcs8 -topk8 -nocrypt -out "$secrets/apns_key.p8"
  chmod 0600 "$secrets/apns_key.p8"
  openssl rand -hex 32 > "$RUN/private/mcp/cursor_key"
  chmod 0444 "$RUN/private/mcp/cursor_key"
  printf '{"export.v1":"%s"}\n' "$(openssl rand -hex 32)" > "$RUN/private/keyrings/export-keys.json"
  chmod 0600 "$RUN/private/keyrings/export-keys.json"
  : > "$RUN/private/compose.env"
}

# check_db_marker DB: the babytracker database carries the load-stack
# marker (set by up; the seeder refuses a database without it).
check_db_marker() {
  local marker
  marker=$(docker exec "$1" psql -XAt -U postgres -d postgres -c \
    "SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = 'babytracker'")
  [[ $marker == "$load_stack_marker" ]] || fail "the babytracker database does not carry the load-stack marker (got '$marker')"
}

cmd_up() {
  resolve_run 0
  bt_load_require_lock "$RUN" || exit 1
  guard_out private source-sha source.json
  [[ ! -e $RUN/private ]] || fail "the run is already up ($RUN/private exists); use a new run"
  local tool dirty db
  for tool in docker jq openssl curl; do command -v "$tool" >/dev/null || fail "load-node needs $tool"; done
  write_secrets
  source_sha=$(/usr/bin/git -C "$repo_root" rev-parse HEAD)
  dirty=$(/usr/bin/git -C "$repo_root" status --porcelain --untracked-files=no -- Server | awk 'END { print (NR > 0) ? "true" : "false" }')
  printf '%s\n' "$source_sha" > "$RUN/source-sha"
  jq -n --arg sha "$source_sha" --argjson dirty "$dirty" --arg at "$(now_iso)" --arg project "$project" \
    '{source_sha: $sha, server_tree_dirty: $dirty, up_at: $at, project: $project, build: "identitytest"}' > "$RUN/source.json"
  compose --profile tools config --quiet
  compose up -d --wait --wait-timeout 180 db
  if [[ -d $server_dir/cmd/baby-tracker-load ]]; then
    compose build app migrate loadgen
  else
    echo "note: Server/cmd/baby-tracker-load is not in this tree yet; the generator image is not built" >&2
    compose build app migrate
  fi
  compose run --rm --no-deps migrate
  compose run --rm --no-deps migrate authority init --instance-id "$instance_id" --epoch 1 --confirm-instance "$instance_id"
  db=$(container_of db)
  [[ -n $db ]] || fail "no db container"
  # The marker the seeder requires before it writes (Task 4): only this
  # disposable load stack's database carries it.
  docker exec "$db" psql -XAtq -U postgres -d postgres -c "COMMENT ON DATABASE babytracker IS '$load_stack_marker'"
  check_db_marker "$db"
  verify_caps
  docker exec "$db" psql -XAtq -U postgres -d postgres -c 'CREATE EXTENSION IF NOT EXISTS pg_stat_statements'
  echo "up: $project at $source_sha; caps verified ($RUN/caps.json)"
}

# --- caps ----------------------------------------------------------------------

mismatches=""
mismatch() { mismatches="$mismatches$1"$'\n'; }

# check_inspect SERVICE CONTAINER WANT: HostConfig NanoCpus, Memory, MemorySwap.
inspected=""
check_inspect() {
  local service=$1 id=$2 want=$3 g1 g2 g3 w1 w2 w3
  inspected=$(docker inspect -f '{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}} {{.HostConfig.MemorySwap}}' "$id")
  read -r g1 g2 g3 <<<"$inspected"
  read -r w1 w2 w3 <<<"$want"
  [[ $g1 == "$w1" ]] || mismatch "$service NanoCpus $g1 != $w1"
  [[ $g2 == "$w2" ]] || mismatch "$service Memory $g2 != $w2"
  [[ $g3 == "$w3" ]] || mismatch "$service MemorySwap $g3 != $w3"
}

# The app's own view of its limits: Task 2's startup line
# {"msg":"runtime limits","gomaxprocs":4,"cgroup_memory_max":8589934592,…}.
app_runtime_limits() { app_log_line "$1" "runtime limits"; }

# app_log_line CONTAINER MSG: the last JSON log line with that msg.
app_log_line() {
  docker logs "$1" 2>&1 | jq -Rc --arg msg "$2" 'fromjson? | select(type == "object" and .msg == $msg)' | awk 'END { if (NR) print }'
}

check_app_runtime() {
  local line=$1
  if [[ -z $line ]]; then mismatch "app log has no 'runtime limits' line (Task 2)"; return; fi
  [[ $(jq -r '.gomaxprocs' <<<"$line") == 4 ]] || mismatch "app runtime limits gomaxprocs=$(jq -r '.gomaxprocs' <<<"$line") != 4"
  [[ $(jq -r '.cgroup_memory_max' <<<"$line") == 8589934592 ]] || mismatch "app runtime limits cgroup_memory_max=$(jq -r '.cgroup_memory_max' <<<"$line") != 8589934592"
}

# The burn bound: a container capped like app and db, spinning 8 busy loops,
# must use at most 420% and on average at least 350% over three windows of
# BT_LOAD_BURN_GAP seconds (default 3) after a 10 s start. Over the bound
# means the cap is not enforced; under the floor means it could not even reach
# 4 CPUs (a busy machine). Each window is the cgroup's own CPU counter
# (cpu.stat usage_usec) across a sleep inside the container: docker stats
# samples on OrbStack read 430-450% for a container the cgroup holds at 400%
# (throttled in every period), so they cannot prove the cap either way.
burn_samples="" burn_mean=""
burn_bound() {
  local name=$project-burn delay=${BT_LOAD_BURN_DELAY:-10} gap=${BT_LOAD_BURN_GAP:-3} sample
  [[ $delay =~ ^[0-9]+$ && $gap =~ ^[0-9]+$ ]] || die "invalid burn timing"
  docker rm -f "$name" >/dev/null 2>&1 || true
  burn_started=1
  docker run --rm -d --name "$name" --cpus 4 --memory 8g --memory-swap 8g "$pg_image" \
    sh -c 'for i in 1 2 3 4 5 6 7 8; do (while :; do :; done) & done; sleep 30' >/dev/null
  sleep "$delay"
  burn_samples=""
  for _ in 1 2 3; do
    # shellcheck disable=SC2016 # expanded inside the container
    sample=$(docker exec "$name" sh -c 'u() { awk "/^usage_usec/ { print \$2 }" /sys/fs/cgroup/cpu.stat; }
      a=$(u); sleep "$1"; b=$(u); awk -v a="$a" -v b="$b" -v s="$1" "BEGIN { if (s > 0) printf \"%.2f\", (b - a) / (s * 10000) }"' \
      burn "$gap" | awk 'NR == 1')
    [[ $sample =~ ^[0-9]+(\.[0-9]+)?$ ]] || { mismatch "burn container cpu.stat unreadable: '$sample'"; break; }
    burn_samples="$burn_samples $sample"
  done
  docker rm -f "$name" >/dev/null 2>&1 || true
  burn_started=0
  [[ -n $burn_samples ]] || return 0
  for sample in $burn_samples; do
    awk -v s="$sample" 'BEGIN { exit !(s > 420) }' && mismatch "burn container used $sample% > 420%: the CPU cap is not enforced"
  done
  burn_mean=$(awk -v list="$burn_samples" 'BEGIN { n = split(list, a, " "); for (i = 1; i <= n; i++) t += a[i]; printf "%.2f", t / n }')
  if awk -v m="$burn_mean" 'BEGIN { exit !(m < 350) }'; then
    mismatch "burn mean $burn_mean% < 350%: the capped container could not reach 4 CPUs (machine busy?)"
  fi
  return 0
}

verify_caps() {
  local app db loadgen running app_values db_values loadgen_values="" loadgen_inspected=false config_uncapped
  local cgroup cpu_max memory_max runtime_line="" host_facts vm_facts
  mismatches=""
  burn_samples="" burn_mean=""
  guard_out caps.json
  db=$(container_of db)
  [[ -n $db ]] || fail "no db container; run up first"
  app=$(container_of app)
  if [[ -z $app ]]; then
    compose create --no-build --no-recreate app >/dev/null
    app=$(container_of app)
  fi
  [[ -n $app ]] || fail "could not create the app container"
  check_inspect app "$app" "$capped"
  app_values=$inspected
  check_inspect db "$db" "$capped"
  db_values=$inspected
  # The generator: uncapped in the merged configuration, and in a created
  # container once its image exists (Tasks 4 and 10 add its command).
  config_uncapped=$(compose --profile tools config --format json | jq '.services.loadgen as $l
    | $l != null and ([$l.cpus, $l.mem_limit, $l.memswap_limit, $l.cpu_quota, $l.cpu_count, $l.cpu_percent, $l.cpuset,
        $l.deploy.resources.limits] | all(. == null))')
  [[ $config_uncapped == true ]] || mismatch "loadgen is capped in the merged Compose configuration"
  if docker image inspect "$project-loadgen:identitytest" >/dev/null 2>&1; then
    loadgen=$(container_of loadgen)
    if [[ -z $loadgen ]]; then
      compose create --no-build --no-recreate loadgen >/dev/null
      loadgen=$(container_of loadgen)
    fi
    [[ -n $loadgen ]] || fail "could not create the loadgen container"
    check_inspect loadgen "$loadgen" "$uncapped"
    loadgen_values=$inspected
    loadgen_inspected=true
  fi
  cgroup=$(docker exec "$db" cat /sys/fs/cgroup/cpu.max /sys/fs/cgroup/memory.max)
  cpu_max=$(awk 'NR == 1' <<<"$cgroup")
  memory_max=$(awk 'NR == 2' <<<"$cgroup")
  [[ $cpu_max == "400000 100000" ]] || mismatch "db cgroup cpu.max '$cpu_max' != '400000 100000'"
  [[ $memory_max == 8589934592 ]] || mismatch "db cgroup memory.max '$memory_max' != '8589934592'"
  running=$(docker inspect -f '{{.State.Running}}' "$app" 2>/dev/null || true)
  if [[ $running == true ]]; then
    runtime_line=$(app_runtime_limits "$app")
    check_app_runtime "$runtime_line"
  fi
  if [[ -z $mismatches ]]; then
    burn_bound
  else
    echo "load-node: burn bound skipped: the declared limits already differ" >&2
  fi
  # The VM and host facts the label stands on (decision D2: the two 8 GiB
  # caps are ceilings when the memory they share, OrbStack's 12 GiB or the
  # Linux VM's MemTotal, is smaller than both together).
  host_facts=$(bt_load_host_facts)
  vm_facts=$(bt_load_vm_facts "$host_facts")
  resolve_node_label "$host_facts"
  jq -n --arg at "$(now_iso)" --arg label "$node_label" --arg project "$project" --arg mismatches "$mismatches" \
    --arg app "$app_values" --arg db "$db_values" --arg loadgen "$loadgen_values" --argjson loadgen_inspected "$loadgen_inspected" \
    --argjson config_uncapped "$config_uncapped" --arg cpu_max "$cpu_max" --arg memory_max "$memory_max" \
    --arg runtime "$runtime_line" --arg app_running "$running" --arg burn "$burn_samples" --arg burn_mean "$burn_mean" \
    --argjson vm "$vm_facts" '
    def num: if . == null or . == "" then null else tonumber end;
    def hostconfig: split(" ") | {NanoCpus: (.[0] | num), Memory: (.[1] | num), MemorySwap: (.[2] | num)};
    {ok: ($mismatches == ""), label: $label, verified_at: $at, project: $project,
       mismatches: ($mismatches | split("\n") | map(select(length > 0))),
       declared: {app: {cpus: 4, memory_bytes: 8589934592, memory_swap_bytes: 8589934592},
                  db: {cpus: 4, memory_bytes: 8589934592, memory_swap_bytes: 8589934592}, loadgen: "uncapped"},
       app: (($app | hostconfig) + {running: ($app_running == "true"),
             runtime_limits: (if $runtime == "" then null else ($runtime | fromjson) end)}),
       db: (($db | hostconfig) + {cgroup_cpu_max: $cpu_max, cgroup_memory_max: $memory_max}),
       loadgen: ((if $loadgen_inspected then ($loadgen | hostconfig) else {} end)
                 + {inspected: $loadgen_inspected, config_uncapped: $config_uncapped}),
       burn: {samples_pct: ($burn | split(" ") | map(select(length > 0) | tonumber)), mean_pct: ($burn_mean | num),
              bound_pct: 420, floor_pct: 350, cpus: 4},
       vm: $vm}' > "$RUN/caps.json"
  if [[ -n $mismatches ]]; then
    printf '%s' "$mismatches" | sed 's/^/load-node: caps mismatch: /' >&2
    fail "the reference node is not as declared; see $RUN/caps.json"
  fi
  echo "caps verified: app and db 4 CPU / 8 GiB, loadgen uncapped, burn mean $burn_mean% (samples$burn_samples)"
}

cmd_verify_caps() {
  resolve_run 0
  bt_load_require_lock "$RUN" || exit 1
  load_state
  verify_caps
}

# --- seed, start, run, profile, audit ------------------------------------------------

# The seed's WAL buffers (Task 13 Step 2, W1-D review I5): the care COPY is
# bound by WAL insertion at PostgreSQL's 16 MB default (wal_buffers_full
# about 450k per 3,000-family seed). The seed alone runs with
# BT_LOAD_SEED_WAL_BUFFERS (256MB): ALTER SYSTEM and a restart of the db
# before it, ALTER SYSTEM RESET and a restart after it, also when it fails,
# so every measurement runs on the overlay's settings. Each step goes to
# seed.log and seed-settings.json.
seed_wal_value=""
seed_wal_before=""

# seed_wait_for_seeder: wait until no generator container of this project
# runs, at most BT_LOAD_SEED_RESTORE_WAIT (900) seconds.
seed_wait_for_seeder() {
  local wait n=0
  wait=$(whole_number "${BT_LOAD_SEED_RESTORE_WAIT:-900}" BT_LOAD_SEED_RESTORE_WAIT)
  while [[ -n $(loadgen_containers) ]] && ((n < wait)); do
    if ((n == 0)); then echo "load-node: waiting for the seeder to finish restoring the schema before the db restarts" >&2; fi
    sleep 1
    n=$((n + 1))
  done
}

# check_db_overrides: refuse a db whose wal_buffers was left set by ALTER
# SYSTEM (a seed killed between its set and its reset, on a kept volume):
# the measurements run on the overlay's settings only.
check_db_overrides() {
  local db count
  db=$(container_of db)
  [[ -n $db ]] || fail "no db container"
  count=$(docker exec "$db" psql -XAtq -U postgres -d postgres -c \
    "SELECT count(*) FROM pg_file_settings WHERE sourcefile LIKE '%postgresql.auto.conf' AND name = 'wal_buffers'" 2>/dev/null | awk 'NR == 1')
  [[ $count =~ ^[0-9]+$ ]] || fail "could not read the db's settings (pg_file_settings)"
  ((count == 0)) || fail "the db's wal_buffers is still set by ALTER SYSTEM (a seed was interrupted, now $(db_setting wal_buffers)): run ALTER SYSTEM RESET wal_buffers and restart the db, or seed a new stack"
}

db_setting() {
  docker exec "$(container_of db)" psql -XAtq -U postgres -d postgres -c "SHOW $1" 2>/dev/null | awk 'NR == 1'
}

# restart_db: restart the db and wait until it is healthy again. A
# CHECKPOINT first leaves the shutdown checkpoint next to nothing to flush
# (a seed leaves up to shared_buffers dirty), and the stop gets 120 s, not
# Docker's 10 s: a SIGKILLed postgres would replay WAL on start, past the
# health wait.
restart_db() {
  docker exec "$(container_of db)" psql -XAtq -U postgres -d postgres -c CHECKPOINT >/dev/null 2>&1 || return 1
  compose restart -t 120 db >/dev/null 2>&1 || return 1
  compose up -d --no-build --wait --wait-timeout 600 db >/dev/null 2>&1
}

seed_wal_apply() {
  local db now
  db=$(container_of db)
  [[ -n $db ]] || fail "no db container; run up first"
  seed_wal_before=$(db_setting wal_buffers)
  docker exec "$db" psql -XAtq -U postgres -d postgres -c "ALTER SYSTEM SET wal_buffers = '$seed_wal_value'" >/dev/null ||
    fail "could not set wal_buffers for the seed"
  seed_wal_applied=1
  restart_db || fail "the db did not come back after setting wal_buffers"
  now=$(db_setting wal_buffers)
  [[ $now == "$seed_wal_value" ]] || fail "wal_buffers is $now after the restart, not $seed_wal_value"
  echo "seed: wal_buffers $seed_wal_value for the seed (was $seed_wal_before), applied at $(now_iso)" | tee -a "$RUN/seed.log"
  jq -n --arg value "$seed_wal_value" --arg before "$seed_wal_before" --arg at "$(now_iso)" \
    '{wal_buffers: {during_seed: $value, before: $before, applied_at: $at}}' > "$RUN/seed-settings.json"
}

seed_wal_reset() {
  local db after
  db=$(container_of db)
  [[ -n $db ]] || return 1
  docker exec "$db" psql -XAtq -U postgres -d postgres -c "ALTER SYSTEM RESET wal_buffers" >/dev/null || return 1
  restart_db || return 1
  after=$(db_setting wal_buffers)
  seed_wal_applied=0
  echo "seed: wal_buffers reset to $after after the seed, at $(now_iso)" | tee -a "$RUN/seed.log"
  if [[ -f $RUN/seed-settings.json && ! -L $RUN/seed-settings.json ]]; then
    jq --arg after "$after" --arg at "$(now_iso)" '.wal_buffers += {after: $after, reset_at: $at}' "$RUN/seed-settings.json" > "$RUN/seed-settings.json.tmp" &&
      mv "$RUN/seed-settings.json.tmp" "$RUN/seed-settings.json"
  fi
  if [[ -f $RUN/seed-report.json && ! -L $RUN/seed-report.json && -f $RUN/seed-settings.json ]]; then
    jq --slurpfile settings "$RUN/seed-settings.json" '.DatabaseSettings = $settings[0]' "$RUN/seed-report.json" > "$RUN/seed-report.json.tmp" &&
      mv "$RUN/seed-report.json.tmp" "$RUN/seed-report.json"
  fi
  [[ $after == "$seed_wal_before" ]] || { echo "load-node: wal_buffers is $after after the reset, not $seed_wal_before" >&2; return 1; }
}

cmd_seed() {
  [[ $opt_families =~ ^[0-9]+$ && $opt_families -ge 1 ]] || die "--families N is required"
  [[ $opt_seed =~ ^[0-9]+$ ]] || die "--seed S is required"
  local backlog=${opt_backlog_load:-2} as_of=${opt_as_of:-$(now_iso)} history=${opt_history:-r10k} workers=${opt_workers:-16}
  [[ $backlog =~ ^[0-9]+(\.[0-9]+)?$ ]] || die "invalid --backlog-load"
  [[ $as_of =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$ ]] || die "--as-of must be RFC 3339"
  [[ $history == r10k || $history == small ]] || die "--history must be r10k or small"
  [[ $workers =~ ^[0-9]+$ && $workers -ge 1 && $workers -le 64 ]] || die "invalid --workers"
  seed_wal_value=${BT_LOAD_SEED_WAL_BUFFERS:-256MB}
  [[ $seed_wal_value =~ ^[1-9][0-9]{0,4}(kB|MB|GB)$ ]] || die "BT_LOAD_SEED_WAL_BUFFERS must be like 256MB"
  resolve_run 0
  bt_load_require_lock "$RUN" || exit 1
  load_state
  require_generator
  guard_out seed-report.json seed.log seed-idle.json .seeding seed-settings.json
  bt_load_disk_check "$opt_families" >/dev/null || exit 1
  bt_load_idle_once > "$RUN/seed-idle.json" || exit 1
  local app
  app=$(container_of app)
  if [[ -n $app && $(docker inspect -f '{{.State.Running}}' "$app" 2>/dev/null) == true ]]; then
    fail "the app is running: the seed restarts the db (wal_buffers); seed before start"
  fi
  : > "$RUN/seed.log"
  seed_wal_apply
  # A 10,000-family seed takes minutes: keep the lease fresh meanwhile.
  local renewer status=0 hot=()
  if ((opt_hot_family)); then hot=(--hot-family); fi
  # The superuser seeder sessions' memory (BT_LOAD_SEED_WORK_MEM,
  # BT_LOAD_SEED_MAINTENANCE_WORK_MEM; the seeder validates them): with large
  # shared buffers, 32 sessions at the defaults overflow the db's cap.
  if [[ -n ${BT_LOAD_SEED_WORK_MEM:-} ]]; then hot+=(--work-mem "$BT_LOAD_SEED_WORK_MEM"); fi
  if [[ -n ${BT_LOAD_SEED_MAINTENANCE_WORK_MEM:-} ]]; then hot+=(--maintenance-work-mem "$BT_LOAD_SEED_MAINTENANCE_WORK_MEM"); fi
  touch "$RUN/.seeding"
  (trap - EXIT INT TERM; lease_renewer "$RUN/.seeding") &
  renewer=$!
  compose run --rm --no-deps -T loadgen seed --secrets /run/load/private/secrets --families "$opt_families" --seed "$opt_seed" \
    --as-of "$as_of" --history "$history" --backlog-hours 24 --backlog-load "$backlog" --workers "$workers" \
    --fleet /run/load/private/fleet.jsonl --report /run/load/seed-report.json ${hot[@]+"${hot[@]}"} 2>&1 | tee -a "$RUN/seed.log" || status=$?
  rm -f "$RUN/.seeding"
  wait "$renewer" 2>/dev/null || true
  if ! seed_wal_reset; then
    echo "load-node: could not reset wal_buffers after the seed; the measurements must not run on this stack" >&2
    ((status != 0)) || status=1
  fi
  bt_load_renew_lock "$RUN" || echo "load-node: this run no longer holds $BT_LOAD_LOCK" >&2
  exit "$status"
}

cmd_start() {
  resolve_run 0
  bt_load_require_lock "$RUN" || exit 1
  load_state
  start_app
}

# start_app: the app up from its built image, ready, and seeing the declared
# limits (start, and upgrade after its migration).
start_app() {
  guard_out caps.json tunables.json
  local app binding="" deadline line
  check_db_overrides
  compose up -d --no-build --wait --wait-timeout 300 app
  app=$(container_of app)
  [[ -n $app ]] || fail "no app container"
  # A changed BT_LOAD_* tunable recreates the container: inspect this one.
  mismatches=""
  check_inspect app "$app" "$capped"
  deadline=$(($(date +%s) + 300))
  while :; do
    binding=$(compose port app 8080 2>/dev/null || true)
    if [[ $binding == 127.0.0.1:* ]] && curl --silent --fail --max-time 2 "http://$binding/readyz" >/dev/null; then break; fi
    (($(date +%s) < deadline)) || fail "the app was not ready within 300 s"
    sleep 1
  done
  # Task 2's startup lines: the limits the app sees, and its pprof listener
  # (a refused BT_IDENTITYTEST_PPROF_ADDRESS would otherwise surface only at
  # the first profile, mid-run).
  local pprof="" refused="" log_wait
  log_wait=$(whole_number "${BT_LOAD_START_LOG_WAIT:-30}" BT_LOAD_START_LOG_WAIT)
  line=""
  deadline=$(($(date +%s) + log_wait))
  while [[ -z $line || -z $pprof ]] && (($(date +%s) < deadline)); do
    line=$(app_runtime_limits "$app")
    pprof=$(app_log_line "$app" "pprof listening")
    refused=$(app_log_line "$app" "pprof address refused")$(app_log_line "$app" "pprof listener")
    [[ -z $refused ]] || break
    [[ -n $line && -n $pprof ]] || sleep 1
  done
  check_app_runtime "$line"
  if [[ -n $refused ]]; then
    mismatch "app pprof did not start: $(jq -r '.error // .msg' <<<"$refused" 2>/dev/null | awk 'NR == 1')"
  elif [[ -z $pprof ]]; then
    mismatch "app log has no 'pprof listening' line (BT_IDENTITYTEST_PPROF_ADDRESS, Task 2)"
  fi
  if [[ -f $RUN/caps.json ]]; then
    jq --arg line "$line" --arg pprof "$pprof" --arg mismatches "$mismatches" --arg inspected "$inspected" '
      .app += ($inspected | split(" ") | {NanoCpus: (.[0] | tonumber), Memory: (.[1] | tonumber), MemorySwap: (.[2] | tonumber)})
      | .app.running = true
      | .app.runtime_limits = (if $line == "" then null else ($line | fromjson) end)
      | .app.pprof = (if $pprof == "" then null else ($pprof | fromjson) end)
      | .mismatches += ($mismatches | split("\n") | map(select(length > 0)))
      | .ok = (.mismatches | length == 0)' "$RUN/caps.json" > "$RUN/caps.json.tmp"
    mv "$RUN/caps.json.tmp" "$RUN/caps.json"
  fi
  tunables_json > "$RUN/tunables.json"
  if [[ -n $mismatches ]]; then
    printf '%s' "$mismatches" | sed 's/^/load-node: caps mismatch: /' >&2
    fail "the app does not see the declared limits; see $RUN/caps.json"
  fi
  echo "start: app ready at http://$binding (runtime limits: $line)"
}

# --- upgrade -------------------------------------------------------------------

# upgrade moves an up run to the current tree on its seeded volume (Task 21's
# A/B: one seed, two builds): the images are built from HEAD, the app is
# stopped, the migrate service migrates the existing database (timed: Task
# 22's review times 000037 on the 10k database), and the app is started as
# start starts it. The db container is kept, so a changed db command line in
# the overlay does not apply unless --recreate-db recreates it. upgrade.json
# holds the upgrade's record (status in_progress until it ends), and every
# upgrade that ends, ok or failed, is appended to upgrades.jsonl. (Its state,
# upgrade_record, upgrade_pending and upgrade_stage, is with the cleanup's.)

schema_version() {
  docker exec "$1" psql -XAtq -U postgres -d babytracker -c 'SELECT coalesce(max(version), 0) FROM schema_migrations' 2>/dev/null | awk 'NR == 1'
}

image_id() { docker image inspect -f '{{.Id}}' "$1" 2>/dev/null | awk 'NR == 1'; }

# upgrade_set KEY JSON: one top-level field of the record.
upgrade_set() {
  upgrade_record=$(jq -c --arg key "$1" --argjson value "$2" '.[$key] = $value' <<<"$upgrade_record")
}

# upgrade_write STATUS MESSAGE: upgrade.json is the record so far; an ended
# upgrade (any status but in_progress) is also appended to upgrades.jsonl.
upgrade_write() {
  upgrade_record=$(jq -c --arg status "$1" --arg message "$2" --arg stage "$upgrade_stage" --arg at "$(now_iso)" \
    '. + {status: $status, stage: $stage, message: $message, updated_at: $at}' <<<"$upgrade_record")
  jq . <<<"$upgrade_record" > "$RUN/upgrade.json"
  if [[ $1 != in_progress ]]; then
    printf '%s\n' "$upgrade_record" >> "$RUN/upgrades.jsonl"
    upgrade_pending=0
  fi
}

upgrade_fail() {
  upgrade_write failed "$1"
  fail "$1"
}

cmd_upgrade() {
  resolve_run 0
  bt_load_require_lock "$RUN" || exit 1
  load_state
  guard_out upgrade.json upgrades.jsonl upgrade-migrate.log source-sha source.json
  local db old_sha new_sha dirty before after started ms build=(app migrate)
  [[ ! -e $RUN/.sampling && ! -e $RUN/.seeding ]] || fail "a measurement or seed is in progress on $RUN; upgrade between them"
  [[ -z $(loadgen_containers) ]] || fail "a generator is running ($(loadgen_containers | tr '\n' ' ')); upgrade between measurements"
  db=$(container_of db)
  [[ -n $db && $(docker inspect -f '{{.State.Running}}' "$db" 2>/dev/null) == true ]] || fail "the db is not running: the run must be up"
  check_db_marker "$db"
  old_sha=$source_sha
  new_sha=$(/usr/bin/git -C "$repo_root" rev-parse HEAD)
  # As up: a dirty server tree is recorded, not refused.
  dirty=$(/usr/bin/git -C "$repo_root" status --porcelain --untracked-files=no -- Server | awk 'END { print (NR > 0) ? "true" : "false" }')
  before=$(schema_version "$db")
  [[ $before =~ ^[0-9]+$ ]] || fail "could not read the schema version (schema_migrations)"
  upgrade_record=$(jq -cn --arg at "$(now_iso)" --argjson recreate "$( ((opt_recreate_db)) && echo true || echo false)" \
    --arg old_sha "$old_sha" --arg old_server "$(image_id "$project-server:identitytest")" \
    --arg old_loadgen "$(image_id "$project-loadgen:identitytest")" --argjson before "$before" \
    --arg new_sha "$new_sha" --argjson dirty "$dirty" '
    {started_at: $at, recreate_db: $recreate,
     from: {source_sha: $old_sha, server_image: $old_server, loadgen_image: $old_loadgen, schema_version: $before},
     to: {source_sha: $new_sha, server_tree_dirty: $dirty}}')
  upgrade_pending=1
  upgrade_stage=build
  upgrade_write in_progress "building $new_sha"

  # The images, from HEAD; the app keeps running the old one until it stops.
  source_sha=$new_sha
  if [[ -d $server_dir/cmd/baby-tracker-load ]]; then build+=(loadgen); fi
  compose build "${build[@]}" || upgrade_fail "the build failed; the app was not stopped (it still runs $old_sha)"
  printf '%s\n' "$new_sha" > "$RUN/source-sha"
  if [[ -f $RUN/source.json ]]; then
    jq --arg sha "$new_sha" --argjson dirty "$dirty" --arg at "$(now_iso)" --arg old "$old_sha" \
      '.source_sha = $sha | .server_tree_dirty = $dirty | .upgraded_at = $at | .upgraded_from = ((.upgraded_from // []) + [$old])' \
      "$RUN/source.json" > "$RUN/source.json.tmp" && mv "$RUN/source.json.tmp" "$RUN/source.json"
  fi
  upgrade_record=$(jq -c --arg server "$(image_id "$project-server:identitytest")" --arg loadgen "$(image_id "$project-loadgen:identitytest")" \
    '.to += {server_image: $server, loadgen_image: $loadgen}' <<<"$upgrade_record")

  upgrade_stage=stop_app
  upgrade_write in_progress "stopping the app"
  compose stop -t 30 app || upgrade_fail "the app did not stop"

  if ((opt_recreate_db)); then
    # As the seed's db restart: a CHECKPOINT first, so the stop (120 s, not
    # Docker's 10 s) has next to nothing to flush.
    upgrade_stage=recreate_db
    upgrade_write in_progress "recreating the db"
    docker exec "$db" psql -XAtq -U postgres -d postgres -c CHECKPOINT >/dev/null || upgrade_fail "the CHECKPOINT before the db recreate failed; the app is stopped"
    compose up -d --no-build --force-recreate --timeout 120 --wait --wait-timeout 600 db ||
      upgrade_fail "the db did not come back healthy after its recreate; the app is stopped"
    db=$(container_of db)
    mismatches=""
    check_inspect db "$db" "$capped"
    [[ -z $mismatches ]] || upgrade_fail "the recreated db is not capped as declared: ${mismatches//$'\n'/; }"
    check_db_marker "$db"
  else
    echo "note: the db container is kept: a changed db command line in the overlay (settings such as track_wal_io_timing) does not apply; use --recreate-db for that" >&2
  fi
  check_db_overrides

  upgrade_stage=migrate
  upgrade_write in_progress "migrating"
  started=$(epoch_ms)
  if ! compose run --rm --no-deps -T migrate > "$RUN/upgrade-migrate.log" 2>&1; then
    ms=$(($(epoch_ms) - started))
    after=$(schema_version "$db")
    upgrade_set migration "$(jq -cn --arg before "$before" --arg after "$after" --argjson ms "$ms" \
      '{schema_before: ($before | tonumber? // null), schema_after: ($after | tonumber? // null), duration_ms: $ms, ok: false}')"
    sed 's/^/  migrate: /' "$RUN/upgrade-migrate.log" >&2
    upgrade_fail "the migration failed after $ms ms: the app is stopped (schema version $before before, ${after:-unknown} now; see upgrade-migrate.log); fix the tree and run upgrade again, or down"
  fi
  ms=$(($(epoch_ms) - started))
  after=$(schema_version "$db")
  [[ $after =~ ^[0-9]+$ ]] || upgrade_fail "could not read the schema version after the migration; the app is stopped"
  upgrade_set migration "$(jq -cn --argjson before "$before" --argjson after "$after" --argjson ms "$ms" \
    '{schema_before: $before, schema_after: $after, duration_ms: $ms, ok: true}')"
  check_db_marker "$db"
  upgrade_set db_settings "$(jq -cn --arg wal_io "$(db_setting track_wal_io_timing)" --arg planning "$(db_setting pg_stat_statements.track_planning)" \
    --arg wal_buffers "$(db_setting wal_buffers)" --argjson tuning "$(db_tuning_json)" \
    '{track_wal_io_timing: $wal_io, track_planning: $planning, wal_buffers: $wal_buffers} + $tuning')"

  upgrade_stage=start
  upgrade_write in_progress "starting the app"
  start_app
  upgrade_stage=finished
  upgrade_write ok "upgraded $old_sha to $new_sha"
  bt_load_renew_lock "$RUN" || echo "load-node: this run no longer holds $BT_LOAD_LOCK" >&2
  echo "upgrade: $old_sha -> $new_sha; schema $before -> $after (migration $ms ms); db $( ((opt_recreate_db)) && echo recreated || echo kept)"
}

# record_profile_failure LABEL KIND ERROR: logged, and recorded in
# profile-failures.jsonl (the measurement goes on).
record_profile_failure() {
  echo "load-node: profile $1 $2 failed: $3" >&2
  if [[ ! -L $RUN/profile-failures.jsonl ]]; then
    jq -cn --arg at "$(now_iso)" --arg label "$1" --arg kind "$2" --arg error "$3" \
      '{at: $at, label: $label, kind: $kind, error: $error}' >> "$RUN/profile-failures.jsonl" || true
  fi
}

# profile_capture LABEL: the five profiles from the published pprof port.
# It never exits: a failed capture is recorded, the other kinds are still
# taken, and it returns non-zero, so the run's later offsets go on.
profile_capture() {
  local label=$1 binding dir=$RUN/profiles entry kind path ext target failed=0
  if [[ ! $label =~ ^[A-Za-z0-9._-]+$ ]]; then record_profile_failure "$label" all "invalid label"; return 1; fi
  if [[ -L $dir ]]; then record_profile_failure "$label" all "symlinked profiles directory refused"; return 1; fi
  binding=$(compose port app 6060 2>/dev/null || true)
  if [[ $binding != 127.0.0.1:* ]]; then record_profile_failure "$label" all "the app's pprof port is not published"; return 1; fi
  if [[ ! -d $dir ]] && ! mkdir -m 0755 "$dir"; then record_profile_failure "$label" all "could not create $dir"; return 1; fi
  for entry in "cpu|/debug/pprof/profile?seconds=30|pb.gz" "heap|/debug/pprof/heap|pb.gz" "mutex|/debug/pprof/mutex|pb.gz" \
    "block|/debug/pprof/block|pb.gz" "goroutine|/debug/pprof/goroutine?debug=1|txt"; do
    IFS='|' read -r kind path ext <<<"$entry"
    target=$dir/$label-$kind.$ext
    if [[ -L $target ]]; then record_profile_failure "$label" "$kind" "symlinked output refused"; failed=1; continue; fi
    if ! curl --silent --show-error --fail --max-time 60 -o "$target" "http://$binding$path"; then
      rm -f "$target"
      record_profile_failure "$label" "$kind" "fetching $path failed"
      failed=1
    fi
  done
  if ((failed)); then return 1; fi
  echo "profiles: $dir/$label-{cpu,heap,mutex,block}.pb.gz and $label-goroutine.txt"
}

cmd_profile() {
  [[ -n $opt_label ]] || die "--label X is required"
  [[ $opt_label =~ ^[A-Za-z0-9._-]+$ ]] || die "invalid profile label"
  resolve_run 0
  bt_load_require_lock "$RUN" || exit 1
  load_state
  guard_out profiles profile-failures.jsonl
  profile_capture "$opt_label" || exit 1
}

cmd_audit() {
  resolve_run 0
  bt_load_require_lock "$RUN" || exit 1
  load_state
  require_generator
  guard_out audit.json audit.log verdict.json result.json host.json thresholds.json
  # The run's machine-state verdict goes into the PASS verdict: a
  # contaminated run fails, and a run without a host record is not audited.
  [[ -f $RUN/host.json ]] || fail "no host.json in $RUN: run the measurement first"
  [[ -f $RUN/result.json ]] || fail "no result.json in $RUN: run the measurement first"
  # The current measurement's ledger and tracked devices (archiving moves an
  # earlier measurement's with its result.json): never an older run's. The
  # measurement's run-idle.json dates it; without one nothing is dated.
  [[ -f $RUN/run-idle.json ]] || fail "no run-idle.json in $RUN: run the measurement first"
  local name
  for name in $private_measurement_files; do
    guard_out "private/$name"
    [[ -f $RUN/private/$name ]] || fail "no private/$name in $RUN: the measurement wrote none"
    [[ ! $RUN/private/$name -ot $RUN/run-idle.json ]] || fail "private/$name is older than this measurement"
  done
  local audit_args=(audit --superuser-url-file /run/load/private/secrets/superuser_database_url
    --ledger /run/load/private/ledger.jsonl --tracked /run/load/private/tracked.jsonl
    --out /run/load/audit.json --result /run/load/result.json --host /run/load/host.json --verdict /run/load/verdict.json)
  # The thresholds the run was judged by (run copies --thresholds there).
  if [[ -f $RUN/thresholds.json ]]; then audit_args+=(--thresholds /run/load/thresholds.json); fi
  guard_out verdict.json.tmp
  resolve_node_label
  rm -f "$RUN/verdict.json"
  compose run --rm --no-deps -T loadgen "${audit_args[@]}" 2>&1 | tee "$RUN/audit.log"
  [[ -f $RUN/verdict.json ]] || fail "the audit wrote no verdict.json"
  # The verdict names the node it was measured on.
  jq --arg label "$node_label" '. + {label: $label}' "$RUN/verdict.json" > "$RUN/verdict.json.tmp" &&
    mv "$RUN/verdict.json.tmp" "$RUN/verdict.json"
  jq -r '"verdict: \(if .pass then "PASS" else "FAIL" end) on \(.label)", (.reasons[] | "  \(.)")' "$RUN/verdict.json"
}

readonly measurement_files="host-samples.jsonl docker-stats.jsonl pg-locks.jsonl pg-wait-events.jsonl pg_stat_statements.json pg-stat-cluster.jsonl app.log
  result.json audit.json audit.log verdict.json host.json resources.json measurement.json loadgen.log window-start run-idle.json thresholds.json profiles profile-failures.jsonl
  restart.json backlog.jsonl apns.jsonl apns-samples.jsonl apns.json"

# The generator's private per-measurement files: the write ledger and the
# tracked devices (it creates both and never replaces either). The fleet
# stays: it is the run's, checkpointed by every measurement.
readonly private_measurement_files="ledger.jsonl tracked.jsonl"

# A second measurement in the same run (Task 13 runs 1x, then 2x on the warm
# fleet) moves the previous one's files into runs/<n>/, its private files
# into runs/<n>/private/ (0700), so the next generator starts a new ledger
# and the audit reads the current measurement's.
archive_previous_measurement() {
  local name n=1 target found=0
  for name in $measurement_files; do [[ -e $RUN/$name ]] && found=1; done
  for name in $private_measurement_files; do [[ -e $RUN/private/$name ]] && found=1; done
  ((found)) || return 0
  guard_out runs
  for name in $private_measurement_files; do guard_out "private/$name"; done
  [[ -d $RUN/runs ]] || mkdir -m 0700 "$RUN/runs"
  while [[ -e $RUN/runs/$n ]]; do n=$((n + 1)); done
  target=$RUN/runs/$n
  mkdir -m 0700 "$target" "$target/private"
  for name in $measurement_files; do
    if [[ -e $RUN/$name ]]; then mv "$RUN/$name" "$target/"; fi
  done
  for name in $private_measurement_files; do
    if [[ -e $RUN/private/$name ]]; then mv "$RUN/private/$name" "$target/private/"; fi
  done
  echo "note: the previous measurement moved to $target" >&2
}

# verify_generator_address: the generator's one-off container must sit at
# $generator_ip on the project's load network (compose run has to honour the
# service's ipv4_address). Waits up to 60 s for the container to appear.
generator_address=""
verify_generator_address() {
  local id="" deadline
  deadline=$(($(date +%s) + 60))
  while :; do
    id=$(loadgen_containers | awk 'NR == 1')
    [[ -z $id ]] || break
    if ! kill -0 "$generator_pid" 2>/dev/null; then
      # It exited before its container could be inspected: its own error,
      # not an address, is the reason.
      echo "load-node: the generator exited at once: $(awk '/level=ERROR/' "$RUN/loadgen.log" | tail -n 1)" >&2
      return 1
    fi
    if (($(date +%s) >= deadline)); then
      echo "load-node: could not find the generator container to check its address" >&2
      return 1
    fi
    sleep 1
  done
  generator_address=$(docker inspect -f "{{with index .NetworkSettings.Networks \"${project}_load\"}}{{.IPAddress}}{{end}}" "$id" 2>/dev/null || true)
  if [[ $generator_address != "$generator_ip" ]]; then
    echo "load-node: generator address on ${project}_load is '$generator_address', not $generator_ip (BT_TRUSTED_PROXY_CIDRS)" >&2
    return 1
  fi
}

# app_log_follower CONTAINER FILE SENTINEL SINCE: appends the container's log
# to FILE as it is written (docker logs --follow), so Docker's json-file
# rotation cannot drop the start of a long measurement: a final `docker logs`
# only returns what the rotation still holds (5 x 20 MB by the daemon's
# default, minutes of a loaded app). When the follow ends (the container
# stopped or was restarted, the restart scenario) it attaches again from the
# second it ended, so a restart's lines are kept; lines of that one second
# may then appear twice, never be missing. When SENTINEL goes away it keeps
# following for BT_LOAD_LOG_DRAIN (2) more seconds, the app's last lines
# reaching the file, then stops. Like every background loop here it also stops
# when the runner ($$) is gone.
app_log_follower() {
  local container=$1 file=$2 sentinel=$3 since=$4 drain=${BT_LOAD_LOG_DRAIN:-2} child gone
  if ! [[ $drain =~ ^[0-9]{1,3}$ ]]; then drain=2; fi
  drain=$((10#$drain))
  while [[ -e $sentinel ]] && kill -0 "$$" 2>/dev/null; do
    docker logs --follow --since "$since" "$container" >> "$file" 2>&1 &
    child=$!
    gone=0
    while kill -0 "$child" 2>/dev/null; do
      if [[ ! -e $sentinel ]] || ! kill -0 "$$" 2>/dev/null; then
        if ((gone >= drain)); then kill "$child" 2>/dev/null || true; break; fi
        gone=$((gone + 1))
      fi
      sleep 1
    done
    wait "$child" 2>/dev/null || true
    since=$(now_iso)
    if [[ -e $sentinel ]]; then sleep 1; fi
  done
}

# apns_sampler OUT SAMPLES SENTINEL APP: the identitytest APNs recorder's new
# records every BT_LOAD_APNS_INTERVAL (10) seconds into OUT (Task 18), and
# one line per sample into SAMPLES. Each sample asks only for the records
# after the last it accounted for (?after=last), from the app's published
# port (looked up each time: a restart may move it).
#   - The recorder keeps a ring of its latest 4,096 requests and counts all
#     it took (total): the records it no longer held are counted lost
#     (total - last - returned), and last becomes total, so a wrapped or
#     quiet interval still moves on.
#   - A new recorder (the app restarted: its total fell below last, or the
#     container APP started again) is a new epoch: the sampler asks it for
#     everything (?after=0) in the same sample. What the old recorder took
#     after its last sample is unknown (lost_unknown_at_restart).
#   - in_flight_peak is the recorder's own peak of concurrent requests.
apns_sampler() {
  local out=$1 samples=$2 alive=$3 app=$4 binding body=$RUN/.apns-body.json last=0 epoch=1 started="" cstart at
  local parsed total peak n restarted prev lost lost_error carry_restart=false carry_prev=0
  while [[ -e $alive ]] && kill -0 "$$" 2>/dev/null; do
    binding=$(compose port app 8080 2>/dev/null || true)
    at=$(now_iso)
    cstart=$(docker inspect -f '{{.State.StartedAt}}' "$app" 2>/dev/null || true)
    total=""
    if [[ $binding == 127.0.0.1:* ]] && apns_fetch "$binding" "$last" "$body"; then
      parsed=$(apns_counts "$body")
      read -r total peak n <<<"$parsed"
    fi
    if [[ $total =~ ^[0-9]+$ && $peak =~ ^[0-9]+$ && $n =~ ^[0-9]+$ ]]; then
      # A restart whose re-read failed is carried to the next sample written.
      restarted=$carry_restart prev=$carry_prev
      if [[ $carry_restart == false ]]; then prev=$last; fi
      if ((total < last)) || [[ -n $started && -n $cstart && $cstart != "$started" ]]; then
        if [[ $carry_restart == false ]]; then prev=$last; fi
        restarted=true
        epoch=$((epoch + 1))
        last=0
        total=""
        if apns_fetch "$binding" 0 "$body"; then
          parsed=$(apns_counts "$body")
          read -r total peak n <<<"$parsed"
        fi
      fi
      if [[ -n $cstart ]]; then started=$cstart; fi
      if [[ $total =~ ^[0-9]+$ && $peak =~ ^[0-9]+$ && $n =~ ^[0-9]+$ ]]; then
        bt_load_apns_records "$body" "$last" "$at" "$epoch" >> "$out" 2>/dev/null || true
        lost=$((total - last - n)) lost_error=false
        if ((lost < 0)); then lost=0 lost_error=true; fi
        jq -cn --arg at "$at" --argjson epoch "$epoch" --argjson after "$last" --argjson total "$total" --argjson returned "$n" \
          --argjson lost "$lost" --argjson peak "$peak" --argjson restarted "$restarted" --argjson prev "$prev" --argjson lost_error "$lost_error" '
          {sampled_at: $at, recorder_epoch: $epoch, after: $after, total: $total, returned: $returned, lost: $lost, in_flight_peak: $peak}
          + (if $restarted then {restarted: true, prev_last: $prev, lost_unknown_at_restart: true} else {} end)
          + (if $lost_error then {lost_error: true} else {} end)' >> "$samples"
        last=$total
        carry_restart=false
      elif [[ $restarted == true ]]; then
        carry_restart=true carry_prev=$prev
      fi
    fi
    rm -f "$body"
    bt_load_sleep_while "$alive" "${BT_LOAD_APNS_INTERVAL:-10}"
  done
}

# apns_fetch BINDING AFTER BODY: the recorder's records after sequence AFTER.
apns_fetch() {
  curl --silent --fail --max-time 20 -o "$3" "http://$1/__identitytest/apns?after=$2" 2>/dev/null
}

# apns_counts BODY: "total in_flight_peak returned" of a recorder answer.
apns_counts() {
  jq -r '"\(.total // "x") \(.in_flight_peak // 0) \(.requests // [] | length)"' "$1" 2>/dev/null || echo "x x x"
}

# epoch_ms: the current time in milliseconds (jq's clock: the stock date has
# no sub-second format).
epoch_ms() { jq -n 'now * 1000 | floor'; }

# passthrough_value NAME: the value of --NAME among the generator's options.
passthrough_value() {
  local i
  for ((i = 0; i + 1 < ${#passthrough[@]}; i += 2)); do
    if [[ ${passthrough[i]} == "--$1" ]]; then echo "${passthrough[i + 1]}"; return 0; fi
  done
  return 1
}

# restart_app AT GRACE WINDOW_MS: the restart cell's runner half (Task 23).
# The generator declares the restart window [window-start + AT, + GRACE]
# (WINDOW_MS is window-start in epoch milliseconds): a failure inside it is
# expected, one before it is not. So the stop is issued at the declared start
# plus BT_LOAD_RESTART_MARGIN_MS (250), never before, while the generator
# runs: the app stops with a 30 s grace (SIGTERM, then the graceful stop) and
# starts again at once, then /readyz is awaited. restart.json records the
# times in epoch milliseconds, the stop's offset from the declared start,
# whether stop to ready fit in GRACE, and the container before and after
# (stop and start keep the container, so the app-log follower attaches to
# the same one again). A generator that ends first leaves a restart.json
# that says the restart did not run.
restart_app() {
  local at=$1 grace=$2 window_ms=$3 out=$RUN/restart.json before after issued stopped started ready="" binding deadline status=ok
  local ready_wait margin declared target now
  ready_wait=$(whole_number "${BT_LOAD_RESTART_READY_WAIT:-300}" BT_LOAD_RESTART_READY_WAIT)
  margin=$(whole_number "${BT_LOAD_RESTART_MARGIN_MS:-250}" BT_LOAD_RESTART_MARGIN_MS)
  declared=$((window_ms + at * 1000))
  target=$((declared + margin))
  while :; do
    kill -0 "$$" 2>/dev/null || return 0
    if ! kill -0 "$generator_pid" 2>/dev/null; then
      jq -n --arg status "not run: the generator ended first" --argjson at "$at" --argjson declared "$declared" \
        '{status: $status, restart_at_seconds: $at, declared_start_unix_ms: $declared}' > "$out"
      echo "load-node: the generator ended before the restart" >&2
      return 0
    fi
    now=$(epoch_ms)
    ((now < target)) || break
    if ((target - now > 1500)); then sleep 1; else sleep 0.1; fi
  done
  before=$(container_of app)
  issued=$(epoch_ms)
  compose stop -t 30 app >/dev/null 2>&1 || status="stop failed"
  stopped=$(epoch_ms)
  compose start app >/dev/null 2>&1 || status="start failed"
  started=$(epoch_ms)
  after=$(container_of app)
  deadline=$(($(date +%s) + ready_wait))
  while [[ $status == ok ]]; do
    binding=$(compose port app 8080 2>/dev/null || true)
    if [[ $binding == 127.0.0.1:* ]] && curl --silent --fail --max-time 2 "http://$binding/readyz" >/dev/null 2>&1; then
      ready=$(epoch_ms)
      break
    fi
    if (($(date +%s) >= deadline)); then status="not ready within $ready_wait s"; break; fi
    sleep 0.2
  done
  if ((issued < declared)) && [[ $status == ok ]]; then status="stopped before the declared window"; fi
  jq -n --argjson at "$at" --argjson grace "$grace" --argjson window_ms "$window_ms" --argjson declared "$declared" --argjson margin "$margin" \
    --argjson issued "$issued" --argjson stopped "$stopped" --argjson started "$started" --arg ready "$ready" --arg before "$before" \
    --arg after "$after" --arg status "$status" '
    def iso: (. / 1000 | floor | todate | sub("Z$"; "")) + "." + ((. % 1000) | tostring | ("00" + .)[-3:]) + "Z";
    ($ready | if . == "" then null else tonumber end) as $r
    | {status: $status, restart_at_seconds: $at, restart_grace_seconds: $grace, margin_ms: $margin,
       window_start_unix_ms: $window_ms, declared_start_unix_ms: $declared, stop_offset_ms: ($issued - $declared),
       stop_issued_unix_ms: $issued, stopped_unix_ms: $stopped, started_unix_ms: $started, ready_unix_ms: $r,
       stop_issued_at: ($issued | iso), stopped_at: ($stopped | iso), started_at: ($started | iso), ready_at: ($r | if . == null then null else iso end),
       stop_seconds: (($stopped - $issued) / 1000), start_seconds: (($started - $stopped) / 1000),
       stop_to_ready_seconds: (if $r == null then null else ($r - $issued) / 1000 end),
       within_grace: (if $r == null then false else ($r - $declared) <= $grace * 1000 end),
       container_before: $before, container_after: $after, same_container: ($before != "" and $before == $after)}' > "$out"
  echo "restart: app stopped and started (status $status); see $out" >&2
}

# window_start_ms FILE: the generator's window-start (RFC 3339 with up to
# nanoseconds) in epoch milliseconds, the fraction kept; empty when the file
# holds no time.
window_start_ms() {
  jq -Rr 'capture("^(?<b>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(?<f>\\.[0-9]+)?Z$")
    | ((.b + "Z") | fromdateiso8601) * 1000 + ((("0" + (.f // ".0")) | tonumber) * 1000 | floor)' "$1" 2>/dev/null | awk 'NR == 1'
}

stop_samplers() {
  local pid
  rm -f "$RUN/.sampling"
  for pid in $sampler_pids; do wait "$pid" 2>/dev/null || true; done
  sampler_pids=""
  sampling=0
}

cmd_run() {
  local scenarios=" soak ramp storm restart mass-snapshot hot-family flood import export "
  [[ $scenarios == *" $opt_scenario "* && -n $opt_scenario ]] || die "--scenario must be one of:$scenarios"
  [[ $opt_load =~ ^[0-9]+(\.[0-9]+)?$ ]] || die "--load L is required"
  [[ $opt_minutes =~ ^[0-9]+$ && $opt_minutes -ge 1 && $opt_minutes -le 240 ]] || die "--minutes M (1-240) is required"
  [[ -n $opt_warmup ]] || die "--warmup D is required"
  local warmup_seconds offsets="" offset seconds seed=${opt_seed:-1} window_grace=${BT_LOAD_WINDOW_GRACE:-600} restart_at="" restart_value
  local restart_grace=60
  warmup_seconds=$(duration_seconds "$opt_warmup")
  if [[ $opt_scenario == restart ]]; then
    restart_value=$(passthrough_value restart-at) || die "--scenario restart needs --restart-at D (Ns, Nm or Nh, from window-start)"
    [[ $restart_value =~ ^[0-9]{1,6}[smh]$ ]] || die "--restart-at must be Ns, Nm or Nh (the runner times the restart), not $restart_value"
    restart_at=$(duration_seconds "$restart_value")
    if restart_value=$(passthrough_value restart-grace); then
      [[ $restart_value =~ ^[0-9]{1,6}[smh]$ ]] || die "--restart-grace must be Ns, Nm or Nh (the runner checks the restart against it), not $restart_value"
      restart_grace=$(duration_seconds "$restart_value")
    fi
    # The whole declared window, stop to ready, inside the measurement: the
    # app must be back before the generator's final pulls.
    ((restart_at >= 1 && restart_at + restart_grace <= opt_minutes * 60)) ||
      die "--restart-at plus --restart-grace ($restart_grace s) must fall inside the $opt_minutes-minute measurement"
  fi
  window_grace=$(whole_number "$window_grace" BT_LOAD_WINDOW_GRACE)
  [[ $seed =~ ^[0-9]+$ ]] || die "invalid --seed"
  if [[ -n $opt_profile_at ]]; then
    local profile_list="" step_offsets="" entry step_count step_seconds step
    for entry in ${opt_profile_at//,/ }; do
      [[ $entry == steps ]] || { profile_list="$profile_list $entry"; continue; }
      # One profile in each ramp step, its 30 s CPU profile ending 15 s
      # before the step does (Task 24: the last passing step and the first
      # failing one, which are known only afterwards).
      [[ $opt_scenario == ramp ]] || die "--profile-at steps needs --scenario ramp"
      entry=$(passthrough_value steps) || die "--profile-at steps needs --steps"
      step_count=$(awk -F, '{ print NF }' <<<"$entry")
      entry=$(passthrough_value step-duration) || entry=10m
      [[ $entry =~ ^[0-9]{1,6}[smh]$ ]] || die "--profile-at steps needs --step-duration Ns, Nm or Nh, not $entry"
      step_seconds=$(duration_seconds "$entry")
      ((step_seconds >= 45)) || die "--profile-at steps needs a --step-duration of at least 45s"
      for ((step = 1; step <= step_count; step++)); do
        seconds=$((step * step_seconds - 45))
        ((seconds + 30 <= opt_minutes * 60)) || break
        profile_list="$profile_list ${seconds}s"
        step_offsets="$step_offsets ${seconds}s (step $step)"
      done
      echo "profile-at steps:$step_offsets"
    done
    for offset in $profile_list; do
      seconds=$(duration_seconds "$offset")
      ((seconds + 30 <= opt_minutes * 60)) || die "--profile-at $offset leaves no room for a 30 s CPU profile in $opt_minutes minutes"
      offsets="$offsets $seconds:$offset"
    done
    # The profiler takes them in turn, so in time order, and once for each
    # time (1m and 60s, or a step's offset named again, are one profile:
    # the first name given).
    offsets=$(tr ' ' '\n' <<<"$offsets" | awk 'NF' | sort -s -t: -k1,1n | awk -F: '!seen[$1]++ { printf " %s", $2 }')
    [[ -z $step_offsets ]] || echo "profile schedule:$offsets"
  fi
  resolve_run 0
  bt_load_require_lock "$RUN" || exit 1
  load_state
  require_generator
  require_app_running
  check_db_overrides
  resolve_node_label
  local db_wal_buffers db_tuning
  db_wal_buffers=$(db_setting wal_buffers)
  db_tuning=$(db_tuning_json)
  if [[ -n $opt_thresholds ]]; then
    [[ -f $opt_thresholds && ! -L $opt_thresholds ]] || die "--thresholds must be a regular file"
    jq empty "$opt_thresholds" || die "--thresholds is not JSON"
  fi
  local db window_start="" window_epoch=0 deadline generator_status=0 profiler_pid="" restart_pid="" contaminated ws_ms window_noticed_ms=0
  db=$(container_of db)
  archive_previous_measurement
  # shellcheck disable=SC2086 # the list is whitespace-separated names
  guard_out $measurement_files .sampling
  # Machine state at the start of every measurement, not only at preflight.
  bt_load_idle_once > "$RUN/run-idle.json" || exit 1
  local generator_args=(run --base "$app_base" --host "$app_host"
    --fleet /run/load/private/fleet.jsonl --ledger /run/load/private/ledger.jsonl --tracked /run/load/private/tracked.jsonl
    --out /run/load/result.json --scenario "$opt_scenario" --load "$opt_load" --warmup "$opt_warmup" --measure "${opt_minutes}m"
    --seed "$seed")
  if [[ -n $opt_thresholds ]]; then
    cp "$opt_thresholds" "$RUN/thresholds.json"
    generator_args+=(--thresholds /run/load/thresholds.json)
  fi
  if ((${#passthrough[@]})); then generator_args+=("${passthrough[@]}"); fi
  # The hot maintenance advances its purge family's recovery window as the
  # stack's superuser (Task 16 Step 4).
  if passthrough_value maintenance >/dev/null; then
    generator_args+=(--superuser-url-file /run/load/private/secrets/superuser_database_url)
  fi
  : > "$RUN/host-samples.jsonl"
  : > "$RUN/docker-stats.jsonl"
  : > "$RUN/pg-locks.jsonl"
  : > "$RUN/pg-wait-events.jsonl"
  : > "$RUN/pg-stat-cluster.jsonl"
  : > "$RUN/app.log"
  : > "$RUN/backlog.jsonl"
  : > "$RUN/apns.jsonl"
  : > "$RUN/apns-samples.jsonl"
  local started_at app
  started_at=$(now_iso)
  app=$(container_of app)
  touch "$RUN/.sampling"
  sampling=1
  # Background workers never run this runner's cleanup (bash resets traps
  # in subshells; the reset is explicit so that never depends on it).
  # The app's whole log for the measurement, from before the generator starts
  # until the samplers stop (see app_log_follower); first, so nothing the app
  # writes at the start is left to a later attach.
  (trap - EXIT INT TERM; app_log_follower "$app" "$RUN/app.log" "$RUN/.sampling" "$started_at") &
  sampler_pids="$sampler_pids $!"
  (trap - EXIT INT TERM; bt_load_host_sampler "$RUN/host-samples.jsonl" "$RUN/.sampling") &
  sampler_pids="$sampler_pids $!"
  (trap - EXIT INT TERM; bt_load_docker_sampler "$RUN/docker-stats.jsonl" "$RUN/.sampling" "$project") &
  sampler_pids="$sampler_pids $!"
  (trap - EXIT INT TERM; bt_load_pg_sampler "$db" "$RUN/pg-locks.jsonl" "$RUN/pg-wait-events.jsonl" "$RUN/.sampling") &
  sampler_pids="$sampler_pids $!"
  # Each minute: the session backlog the retention sweep owes (Task 17) and
  # the APNs sink's new records (Task 18).
  (trap - EXIT INT TERM; bt_load_backlog_sampler "$db" "$RUN/backlog.jsonl" "$RUN/.sampling") &
  sampler_pids="$sampler_pids $!"
  (trap - EXIT INT TERM; apns_sampler "$RUN/apns.jsonl" "$RUN/apns-samples.jsonl" "$RUN/.sampling" "$app") &
  sampler_pids="$sampler_pids $!"
  # The lease is renewed while the run lasts (a run may be longer than it).
  (trap - EXIT INT TERM; lease_renewer "$RUN/.sampling") &
  sampler_pids="$sampler_pids $!"
  compose run --rm --no-deps -T loadgen "${generator_args[@]}" > "$RUN/loadgen.log" 2>&1 &
  generator_pid=$!
  # Only the generator's address is trusted for X-Forwarded-For: anything
  # else makes every simulated device one client, skewed by rate limits.
  if ! verify_generator_address; then
    stop_generator
    fail "refusing the run: the generator is not running at $generator_ip"
  fi
  # The generator writes window-start when warm-up ends: the measurement
  # window, pg_stat_statements and the cluster counters' baseline start there.
  deadline=$(($(date +%s) + warmup_seconds + window_grace))
  while [[ ! -e $RUN/window-start ]]; do
    kill -0 "$generator_pid" 2>/dev/null || break
    if (($(date +%s) >= deadline)); then echo "load-node: no window-start within warm-up + $window_grace s" >&2; break; fi
    sleep 1
  done
  if [[ -e $RUN/window-start ]]; then
    bt_load_pg_statements_reset "$db"
    bt_load_pg_cluster_snapshot "$db" "$RUN/pg-stat-cluster.jsonl" window_start
    window_epoch=$(date +%s)
    window_noticed_ms=$(epoch_ms)
    window_start=$(now_iso)
    if [[ -n $offsets ]]; then
      (
        trap - EXIT INT TERM
        for offset in $offsets; do
          seconds=$(duration_seconds "$offset")
          while (($(date +%s) < window_epoch + seconds)); do kill -0 "$$" 2>/dev/null || exit 0; sleep 1; done
          profile_capture "at-$offset" || echo "load-node: profile at $offset incomplete; continuing" >&2
        done
      ) &
      profiler_pid=$!
    fi
    if [[ -n $restart_at ]]; then
      # From the generator's own window-start, to the millisecond, when it
      # wrote one; else from when the runner noticed the file (never earlier
      # than the generator's).
      ws_ms=$(window_start_ms "$RUN/window-start")
      [[ $ws_ms =~ ^[0-9]+$ ]] || ws_ms=$window_noticed_ms
      (trap - EXIT INT TERM; restart_app "$restart_at" "$restart_grace" "$ws_ms") &
      restart_pid=$!
    fi
  elif kill -0 "$generator_pid" 2>/dev/null; then
    # Timed out: stop the client and the container, never leave it running.
    stop_generator
    generator_status=1
  fi
  if [[ -n $generator_pid ]]; then
    wait "$generator_pid" || generator_status=$?
    generator_pid=""
  fi
  if [[ -z $window_start && $generator_status == 0 ]]; then generator_status=1; fi
  if [[ -n $profiler_pid ]]; then wait "$profiler_pid" || true; fi
  if [[ -n $restart_pid ]]; then wait "$restart_pid" || true; fi
  if [[ -n $window_start ]]; then
    bt_load_pg_statements_dump "$db" "$RUN/pg_stat_statements.json" "$window_start"
    bt_load_pg_cluster_snapshot "$db" "$RUN/pg-stat-cluster.jsonl" window_end
  fi
  stop_samplers
  bt_load_renew_lock "$RUN" || echo "load-node: this run no longer holds $BT_LOAD_LOCK" >&2
  bt_load_evaluate_host "$RUN/host-samples.jsonl" > "$RUN/host.json"
  local caps_file=/dev/null
  [[ ! -f $RUN/caps.json ]] || caps_file=$RUN/caps.json
  bt_load_resources "$RUN/docker-stats.jsonl" "$caps_file" "$node_label" > "$RUN/resources.json"
  bt_load_apns_summary "$RUN/apns.jsonl" "$RUN/apns-samples.jsonl" > "$RUN/apns.json" || echo "load-node: could not summarise apns.jsonl" >&2
  [[ $opt_scenario != flood ]] || flood_evictions "$RUN"
  # The tunables: this run's BT_LOAD_* environment, with the app's own
  # tunables as the running app applied them.
  # tunables_source says which: the app's applied values could not be
  # read, the environment's alone are recorded, and the runner warns.
  local tunables applied tunables_source="app container"
  if ! applied=$(applied_tunables_json "$(container_of app)"); then
    applied='{}' tunables_source="environment only (the app container could not be inspected)"
    echo "load-node: could not read the app's applied tunables; measurement.json records this run's BT_LOAD_* environment only" >&2
  fi
  local applied_db
  if ! applied_db=$(applied_db_tunables_json "$(container_of db)"); then
    applied_db='{}'
    echo "load-node: could not read the db's command line; measurement.json records BT_LOAD_LOG_AUTOVACUUM_MIN_DURATION from this run's environment only" >&2
  fi
  tunables=$(jq -cn --argjson env "$(tunables_json)" --argjson applied "$applied" --argjson db "$applied_db" '$env + $applied + $db')
  jq -n --arg scenario "$opt_scenario" --argjson load "$opt_load" --arg warmup "$opt_warmup" --argjson minutes "$opt_minutes" \
    --arg profile_at "$offsets" --arg started_at "$started_at" --arg window_start "$window_start" --arg ended_at "$(now_iso)" \
    --argjson generator_exit "$generator_status" --argjson tunables "$tunables" --arg tunables_source "$tunables_source" --arg label "$node_label" --arg wal_buffers "$db_wal_buffers" \
    --argjson db_tuning "$db_tuning" \
    --arg source_sha "$source_sha" --arg passthrough "${passthrough[*]+${passthrough[*]}}" --arg generator_address "$generator_address" \
    --argjson profile_failures "$(awk 'END { print NR }' "$RUN/profile-failures.jsonl" 2>/dev/null || echo 0)" '
    {label: $label, source_sha: $source_sha, scenario: $scenario, load: $load, warmup: $warmup, minutes: $minutes,
     profile_at: ($profile_at | split(" ") | map(select(length > 0))), generator_options: $passthrough,
     generator_address: $generator_address, profile_failures: $profile_failures,
     started_at: $started_at, window_start: (if $window_start == "" then null else $window_start end), ended_at: $ended_at,
     generator_exit: $generator_exit, tunables: $tunables, tunables_source: $tunables_source, db_settings: ({wal_buffers: $wal_buffers} + $db_tuning)}' > "$RUN/measurement.json"
  contaminated=$(jq -r '.contaminated' "$RUN/host.json")
  echo "run: $opt_scenario at ${opt_load}x, generator exit $generator_status, host contaminated: $contaminated"
  jq -r '.measured | to_entries[] | "  \(.key): cpu mean \(.value.cpu_mean_pct)% peak \(.value.cpu_peak_pct)%, memory peak \(.value.mem_peak_bytes / 1048576 | floor) MiB"' "$RUN/resources.json"
  jq -r '"  VM: " + (if .vm.host_os == "Linux"
      then "MemTotal \(.vm.memory_limit_bytes // null | if . == null then "?" else . / 1048576 | floor end) MiB (Linux)"
      else "\(.vm.orb_memory_mib // "?") MiB (OrbStack)" end)
    + ", caps app 8192 MiB + db 8192 MiB " + (if .vm.caps_are_ceilings == false then "fit within it" else "are ceilings" end)' "$RUN/resources.json"
  [[ $contaminated == false ]] || echo "load-node: the host was not idle during the run: $(jq -c '.offenders' "$RUN/host.json")" >&2
  exit "$generator_status"
}

# --- down ------------------------------------------------------------------------

cmd_down() {
  resolve_run 0
  bt_load_lock_free_or_ours "$RUN" || exit 1
  local status=0
  # Stop any sampler or lease renewer a killed runner left behind.
  rm -f "$RUN/.sampling" "$RUN/.seeding"
  if [[ -d $RUN/private/secrets && -f $RUN/source-sha ]]; then
    load_state
    if ((opt_keep_volume)); then
      compose --profile tools down --remove-orphans || status=$?
    else
      compose --profile tools down --remove-orphans --volumes || status=$?
    fi
  else
    echo "note: $RUN never came up; nothing to stop" >&2
  fi
  docker rm -f "$project-burn" >/dev/null 2>&1 || true
  bt_load_release_lock "$RUN"
  echo "down: $project stopped$( ((opt_keep_volume)) && echo ', database volume kept'); lock released"
  exit "$status"
}

# --- main ----------------------------------------------------------------------

(($#)) || usage
subcommand=$1
shift
case $subcommand in
  preflight) parse_options "run families" "$@" ;;
  up | verify-caps | start | audit) parse_options "run" "$@" ;;
  seed) parse_options "run families seed backlog-load as-of history workers hot-family" "$@" ;;
  run) parse_options "run scenario load warmup minutes profile-at thresholds seed passthrough" "$@" ;;
  profile) parse_options "run label" "$@" ;;
  down) parse_options "run keep-volume" "$@" ;;
  upgrade) parse_options "run recreate-db" "$@" ;;
  -h | --help | help) usage ;;
  *) usage ;;
esac
normalize_options
RUN=${opt_run:-${BT_LOAD_RUN:-}}
if [[ -n $opt_run && -n ${BT_LOAD_RUN:-} && $opt_run != "$BT_LOAD_RUN" ]]; then die "--run and BT_LOAD_RUN name different runs"; fi
"cmd_${subcommand//-/_}"
