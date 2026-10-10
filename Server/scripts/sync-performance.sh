#!/usr/bin/env bash
# Synthetic, local, no-race timing; foreground mode also measures Swift stores.
# Neither mode measures rendered UI or physical devices.
# Resolve before a verified baseline product root becomes cwd.
performance_control_source="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/performance_control.py"
validate_foreground_artifacts() {
  python3 - "$1" "$2" "$3" <<'FOREGROUND'
import json,math,sys
mode=sys.argv[3]
assert mode in {'all','recovery'}, 'unknown foreground scope'
lines=open(sys.argv[1]).read().splitlines()
cases=['healthy','negative','recovery'] if mode=='all' else ['recovery']
for case in cases:
    assert 'foreground-latency proof: '+case+' complete' in lines, 'missing '+case+' proof'
rows=[json.loads(line.split(': ',1)[1]) for line in lines if line.startswith('foreground-latency sample: ')]
pairs=[json.loads(line.split(': ',1)[1]) for line in lines if line.startswith('foreground-latency pair: ')]
required={'hint_only_override_120s':3,'offline_backlog':1,'access_renewal':1,'background_engine_pull':1}
if mode=='all': required.update(healthy=200,unavailable_live_fallback=1)
assert len(rows)==len(required) and {r['distribution'] for r in rows}==set(required), 'missing/duplicate distributions'
assert {p['distribution'] for p in pairs}==set(required), 'missing/unexpected pairs'
for row in rows:
    group=[p for p in pairs if p['distribution']==row['distribution']]
    count=required[row['distribution']]
    assert row['count']==row['expected']==len(group)==count, 'summary/pair count mismatch'
    assert sorted(p['ordinal'] for p in group)==list(range(1,count+1)), 'duplicate/missing pair ordinal'
    for field in ['durable','observable','publication']:
        values=[p[field+'_seconds'] for p in group]
        assert all(type(v) in (int,float) and math.isfinite(v) and v>=0 for v in values), 'invalid duration'
        p95=sorted(values)[math.ceil(count*.95)-1]
        assert abs(p95-row[field+'_p95_seconds'])<1e-9, 'summary does not match actual pairs'
    if row['distribution']=='healthy':
        assert row['durable_p95_seconds']<1 and row['observable_p95_seconds']<1, 'latency gate'
        assert 0<row['elapsed_seconds']<=120, 'healthy measurement budget'
with open(sys.argv[2], 'w') as output:
    for row in pairs: output.write(json.dumps(row,sort_keys=True)+'\n')
print('foreground-latency proof: '+('complete' if mode=='all' else 'recovery-only complete'))
FOREGROUND
}

# One owner for the Swift process group. A signal handler never calls wait:
# Popen.wait can already hold its lock when the signal arrives.
run_matrix_child() {
  exec python3 "$performance_control_source" supervise "$@"
}

# Sourcing exposes only the artifact validator for resource-free regressions.
if [[ ${BASH_SOURCE[0]} != "$0" ]]; then return 0; fi
set -euo pipefail
runner_started=$(date +%s)
cd "$(dirname "$0")/../.."
[[ ${BT_SYNC_PERF_PROBE:-0} == 1 ]] || { echo "BT_SYNC_PERF_PROBE=1 required" >&2; exit 2; }
for name in BABY_TRACKER_TEST_DATABASE_URL BT_DATABASE_URL BT_DATABASE_URL_FILE BT_PROJECTOR_DATABASE_URL BT_PROJECTOR_DATABASE_URL_FILE BT_PUBLIC_ORIGIN BT_RECOVERY_MAINTENANCE_DATABASE_URL_FILE BT_EXPORT_DATABASE_URL_FILE BT_KEYRING_DIR BT_SECRETS_DIR; do
  [[ -z ${!name:-} ]] || { echo "externally supplied connection or path refused" >&2; exit 2; }
done
harness_root=$PWD
output=""; duration=30; total=1200; mode=socket; foreground_case=all; baseline_source=""
while [[ $# -gt 0 ]]; do
  [[ $# -ge 2 ]] || { echo "missing argument" >&2; exit 2; }
  case $1 in
    --output) output=$2 ;;
    --duration) duration=$2 ;;
    --mode) mode=$2 ;;
    --baseline-source) baseline_source=$2 ;;
    --foreground-case) foreground_case=$2 ;;
    --total-seconds) total=$2 ;;
    *) echo "unknown argument" >&2; exit 2 ;;
  esac
  shift 2
done
[[ $duration =~ ^[0-9]+$ && $duration -ge 30 && $duration -le 60 && $total =~ ^[0-9]+$ && $total -ge 60 && $total -le 1200 ]] || { echo "invalid time budget" >&2; exit 2; }
[[ $foreground_case == all || ($mode == foreground && $foreground_case == recovery) ]] || { echo "invalid foreground case" >&2; exit 2; }
[[ $mode == socket || $mode == foreground || $mode == matrix ]] || { echo "invalid mode" >&2; exit 2; }
if [[ -n $baseline_source ]]; then
  [[ $mode == matrix && $baseline_source == /private/tmp/baby-sync-performance-baseline-d622864 ]] || { echo "unapproved baseline source" >&2; exit 2; }
  [[ ! -L $baseline_source && $(git -C "$baseline_source" rev-parse HEAD) == d62286430734d469124ce3dec631b582c914aee5 && -z $(git -C "$baseline_source" status --porcelain --untracked-files=all) ]] || { echo "baseline must be exact clean pinned source" >&2; exit 2; }
fi
# A fresh, direct child of an existing private temporary directory only. Reject
# symlinks at every component; never overwrite or recursively delete output.
python3 - "$output" <<'DESTINATION'
import os, pathlib, sys
p=pathlib.Path(sys.argv[1])
if not p.is_absolute() or '..' in p.parts or p.exists() or p.is_symlink(): sys.exit('fresh absolute output required')
if not str(p).startswith('/private/tmp/') or not p.parent.is_dir(): sys.exit('output must be under /private/tmp')
for part in [p, *p.parents]:
    if part.is_symlink(): sys.exit('symlink output refused')
os.mkdir(p, 0o700)
DESTINATION
umask 077
raw="$output/.private.log"
: > "$raw"
source Server/scripts/lib/disposable-sync.sh
export BT_DISPOSABLE_PERFORMANCE=1
# No inherited application configuration can silently change quotas/authority.
for name in $(compgen -v BT_); do
  case $name in BT_SYNC_PERF_PROBE|BT_SYNC_PERF_TEST_FAULT|BT_DISPOSABLE_PERFORMANCE) ;; *) unset "$name" ;; esac
done
watchdog=""
run_started=$runner_started
run_deadline=$((run_started + total))
cleanup_reserve=$((total / 3))
[[ $cleanup_reserve -le 60 ]] || cleanup_reserve=60
matrix_deadline=$((run_deadline - cleanup_reserve))
phase=setup
python3 - "$output/source-hashes.json" <<'HASHES'
import hashlib,json,pathlib,sys
paths=[pathlib.Path(p) for p in ['Makefile','Server/scripts/sync-performance.sh','Server/scripts/lib/performance_control.py','Server/scripts/lib/disposable-sync.sh','Packages/BabyTracker/Package.swift']]
for root in ['Server/cmd','Server/internal','Packages/BabyTracker/Sources','Packages/BabyTracker/Tests/FamilySyncClientE2ETests']:
    paths.extend(p for p in pathlib.Path(root).rglob('*') if p.suffix in {'.swift','.go'})
with open(sys.argv[1],'w') as f: json.dump({str(p):hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(paths)},f,sort_keys=True)
HASHES
performance_cleanup() {
  local status=$? cleanup_status=0
  trap - EXIT INT TERM
  # Reserve covers supervisor shutdown, report recovery and all teardown.
  if [[ -n ${disposable_command_pid:-} ]]; then
    kill -TERM "$disposable_command_pid" 2>/dev/null || true
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      kill -0 "$disposable_command_pid" 2>/dev/null || break
      sleep 0.1
    done
    kill -KILL "$disposable_command_pid" 2>/dev/null || true
    wait "$disposable_command_pid" 2>/dev/null || true
    disposable_command_pid=""
  fi
  if [[ -n ${metrics_pid:-} ]]; then
    kill -TERM "$metrics_pid" 2>/dev/null || true
    for _ in 1 2 3 4 5 6 7 8 9 10; do kill -0 "$metrics_pid" 2>/dev/null || break; sleep 0.1; done
    kill -KILL "$metrics_pid" 2>/dev/null || true
    wait "$metrics_pid" 2>/dev/null || true
  fi
  if [[ $mode == matrix ]]; then
    grep '^capacity-matrix ' "$raw" > "$output/matrix.log" || true
    python3 "$harness_root/Server/scripts/lib/performance_control.py" preserve "${BABY_TRACKER_MATRIX_PRIVATE:-$output/no-checkpoints}" "$output/cell-evidence" || status=1
  fi
  if [[ -n ${project:-} && -n ${BT_SECRETS_DIR:-} ]]; then
    # Pointers are private recovery information, never credentials. Leave them
    # and owned secrets intact if the supervised teardown cannot finish.
    python3 - "$output/cleanup-recovery.json" "$project" "$BT_SECRETS_DIR" "$override" "$BT_IMAGE_NAME" "$PWD" <<'CLEANUP_CONFIG'
import json,os,sys
with open(sys.argv[1],'w') as f: json.dump(dict(zip(['project','secrets','override','image','source'],sys.argv[2:])),f)
os.chmod(sys.argv[1],0o600)
CLEANUP_CONFIG
    python3 "$harness_root/Server/scripts/lib/performance_control.py" cleanup "$output/cleanup-recovery.json" "$output" "$run_deadline" || cleanup_status=$?
  fi
  if [[ $mode == matrix ]]; then
    python3 "$harness_root/Server/scripts/lib/performance_control.py" aggregate "$output/matrix.log" "$output/cells.json" "$phase" "$output/build.json" "$output/cell-evidence/control-evidence.jsonl" "$output/cell-evidence" || status=1
  fi
  [[ $status != 0 || $cleanup_status == 0 ]] || status=$cleanup_status
  jq -n --arg phase "$phase" --argjson status "$status" '{phase:$phase,status:$status}' > "$output/status.json"
  rm -f "$raw"
  # The hard global watchdog remains alive throughout teardown.
  [[ -z $watchdog ]] || { kill "$watchdog" 2>/dev/null || true; wait "$watchdog" 2>/dev/null || true; }
  printf 'performance runner finished status=%s; artifacts=%s\n' "$status" "$output"
  exit "$status"
}

trap performance_cleanup EXIT
trap 'exit 130' INT
trap 'exit 124' TERM
# The watchdog signals this process; cleanup always remains in its owner.
watchdog_seconds=$((matrix_deadline - $(date +%s)))
[[ $watchdog_seconds -gt 0 ]] || exit 124
[[ ${BT_SYNC_PERF_TEST_FAULT:-} != timeout ]] || watchdog_seconds=30
python3 -c 'import os,signal,sys,time; time.sleep(int(sys.argv[1])); os.kill(int(sys.argv[2]),signal.SIGTERM); time.sleep(int(sys.argv[3])); os.kill(int(sys.argv[2]),signal.SIGKILL)' "$watchdog_seconds" "$$" "$cleanup_reserve" & watchdog=$!
# The shared helper preserves this performance owner's cleanup traps.
if [[ -n $baseline_source ]]; then cd "$baseline_source"; fi
start_disposable_sync self_hosted normal >> "$raw" 2>&1
# Verify every bind is a file/directory this run created, not a user keyring.
# Named volumes are isolated by the random Compose project.
docker inspect "$app" | python3 -c 'import json,os,sys; root=os.path.realpath(sys.argv[1])+"/"; mounts=json.load(sys.stdin)[0]["Mounts"]; assert all(m["Type"] != "bind" or os.path.realpath(m["Source"]).startswith(root) for m in mounts), "unowned application bind"' "$BT_SECRETS_DIR"
phase=tls
compose up -d --wait --wait-timeout 60 tls >> "$raw" 2>&1
curl --silent --fail --max-time 5 --cacert "$BT_SECRETS_DIR/tls.crt" "$BT_PUBLIC_ORIGIN/readyz" >/dev/null
maintenance=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "SELECT has_function_privilege('baby_tracker_export_app','app.cleanup_refresh_recovery_receipts(timestamptz)','EXECUTE') AND NOT has_function_privilege('baby_tracker_app','app.cleanup_refresh_recovery_receipts(timestamptz)','EXECUTE')")
[[ $maintenance == t ]] || { echo "maintenance privilege proof failed" >&2; exit 1; }
if [[ $mode == matrix ]]; then
  jq -n --arg source "$source_sha" --arg binary "$binary_sha" --arg image "$image_id" --arg harness "$(git -C "$harness_root" rev-parse HEAD)" --arg os "$(uname -sm)" --argjson schema "$schema" '{source:$source,binary_sha:$binary,image:$image,harness_source:$harness,build:"identitytest-no-race",environment:$os,schema:$schema,single_source_ip:true,default_quotas:true,baseline_unmodified:true}' > "$output/build.json"
  python3 - "$output/product-source-hashes.json" <<'PRODUCT_HASHES'
import hashlib,json,pathlib,sys
paths=[p for root in ['Server/cmd','Server/internal','Server/migrations'] for p in pathlib.Path(root).rglob('*') if p.is_file() and p.suffix in {'.go','.sql'}]
json.dump({str(p):hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(paths)},open(sys.argv[1],'w'),sort_keys=True)
PRODUCT_HASHES
fi
phase=recovery_worker
worker_deadline=$((SECONDS + 330))
while ! compose logs --no-color app 2>/dev/null | grep -q 'identitytest recovery cleanup completed'; do
  [[ $SECONDS -lt $worker_deadline ]] || { echo "cleanup worker admission missing" >&2; exit 1; }
  sleep 1
done
phase=client_build
(cd "$harness_root/Server" && go build -tags identitytest -o "$BT_SECRETS_DIR/performance-client" ./cmd/sync-performance-client) >> "$raw" 2>&1
phase=client_measurement
if [[ $mode == matrix ]]; then
  phase=swift_matrix
  export BABY_TRACKER_CAPACITY_MATRIX=1 BABY_TRACKER_E2E_BASE="$BT_PUBLIC_ORIGIN" BABY_TRACKER_E2E_CERT="$BT_SECRETS_DIR/tls.crt" BABY_TRACKER_E2E_INSTANCE="$BT_SERVER_INSTANCE_ID"
  export BABY_TRACKER_MATRIX_LOG="$raw"
  export BABY_TRACKER_MATRIX_PRIVATE="$BT_SECRETS_DIR/matrix" BABY_TRACKER_MATRIX_CLIENT="$BT_SECRETS_DIR/performance-client" BABY_TRACKER_MATRIX_DEADLINE="$matrix_deadline" BABY_TRACKER_MATRIX_SECONDS="$duration"
  mkdir -m 0700 "$BABY_TRACKER_MATRIX_PRIVATE"
  export BABY_TRACKER_MATRIX_EVIDENCE="$BABY_TRACKER_MATRIX_PRIVATE/control-evidence.jsonl"
  export BABY_TRACKER_MATRIX_CONTROL="$BABY_TRACKER_MATRIX_PRIVATE/control.jsonl" BABY_TRACKER_MATRIX_ACK="$BABY_TRACKER_MATRIX_PRIVATE/control.ack"
  : > "$BABY_TRACKER_MATRIX_CONTROL"
  chmod 0600 "$BABY_TRACKER_MATRIX_CONTROL"
  python3 "$harness_root/Server/scripts/lib/performance_control.py" sample-loop "$app" "$db" "$BABY_TRACKER_MATRIX_PRIVATE/resource-samples.jsonl" & metrics_pid=$!

  export BABY_TRACKER_MATRIX_HEAD_READER="$BT_SECRETS_DIR/read-head"
  python3 - "$BABY_TRACKER_MATRIX_HEAD_READER" "$db" <<'HEAD_READER'
import os,shlex,sys
open(sys.argv[1],'w').write('#!/bin/sh\nexec docker exec '+shlex.quote(sys.argv[2])+" psql -XAt -U postgres -d babytracker -c 'SELECT revision FROM security_projection_head'\n")
os.chmod(sys.argv[1],0o700)
HEAD_READER
  (run_matrix_child "$harness_root/Packages/BabyTracker" swift test --filter CapacityMatrixE2ETests/capacityMatrix) >> "$raw" 2>&1 &
  disposable_command_pid=$!
  wait "$disposable_command_pid"
  disposable_command_pid=""
  docker stats --no-stream --format '{{json .}}' "$app" "$db" | python3 -c 'import json,sys; print(json.dumps([{k:r.get(k) for k in ["CPUPerc","MemUsage","NetIO","BlockIO","PIDs"]} for r in map(json.loads,sys.stdin)]))' > "$output/resource-final-sample.json"
  docker exec "$db" psql -XAt -U postgres -d babytracker -c 'SELECT count(*) FROM pg_stat_activity' > "$output/db-connections-final-sample.txt"
elif [[ $mode == foreground ]]; then
  "$BT_SECRETS_DIR/performance-client" --base "$BT_PUBLIC_ORIGIN" --cert "$BT_SECRETS_DIR/tls.crt" --proof-only --state "$BT_SECRETS_DIR/client-state.json" > "$output/socket-proof.jsonl" 2>> "$raw" &
else
  "$BT_SECRETS_DIR/performance-client" --base "$BT_PUBLIC_ORIGIN" --cert "$BT_SECRETS_DIR/tls.crt" --duration "$duration" > "$output/measurements.jsonl" 2>> "$raw" &
fi
if [[ $mode != matrix ]]; then
  disposable_command_pid=$!
  wait "$disposable_command_pid"
  disposable_command_pid=""
fi
if [[ $mode == foreground ]]; then
  phase=swift_foreground
  swift_filter=ForegroundLatencyE2ETests
  [[ $foreground_case == all ]] || swift_filter='ForegroundLatencyE2ETests/foregroundReconnectAndRenewalHaveSeparateDistribution'
  (cd Packages/BabyTracker &&
    BABY_TRACKER_FOREGROUND_LATENCY=1 \
    BABY_TRACKER_E2E_BASE="$BT_PUBLIC_ORIGIN" \
    BABY_TRACKER_E2E_STATE="$BT_SECRETS_DIR/client-state.json" \
    BABY_TRACKER_E2E_CERT="$BT_SECRETS_DIR/tls.crt" \
    BABY_TRACKER_E2E_INSTANCE="$BT_SERVER_INSTANCE_ID" \
    swift test --filter "$swift_filter") >> "$raw" 2>&1 &
  disposable_command_pid=$!
  swift_status=0
  wait "$disposable_command_pid" || swift_status=$?
  disposable_command_pid=""
  grep '^foreground-latency ' "$raw" > "$output/foreground.log" || true
  cat "$output/foreground.log"
  [[ $swift_status == 0 ]] || { echo "foreground Swift suite failed" >&2; exit "$swift_status"; }
  validate_foreground_artifacts "$output/foreground.log" "$output/foreground-pairs.jsonl" "$foreground_case" | tee -a "$output/foreground.log"
fi
phase=artifacts
head=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c 'SELECT revision FROM security_projection_head')
epoch=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c 'SELECT recovery_epoch FROM server_state WHERE singleton')
jq -n --arg source "$source_sha" --arg binary "$binary_sha" --arg image "$image_id" --argjson head "$head" --argjson epoch "$epoch" --argjson schema "$schema" --arg foreground_case "$foreground_case" --argjson peer_store_measured "$([[ $mode == foreground ]] && echo true || echo false)" '{source_sha:$source,binary_sha:$binary,image_id:$image,security_events:$head,recovery_epoch:$epoch,schema:$schema,owned_application_binds:true,mode:"self_hosted",build:"identitytest-no-race",maintenance_isolated:true,cleanup_worker_admitted:true,foreground_case:$foreground_case,peer_store_measured:$peer_store_measured,ui_measured:false}' > "$output/composition.json"
if [[ -n $baseline_source ]]; then
  [[ $(git -C "$baseline_source" rev-parse HEAD) == d62286430734d469124ce3dec631b582c914aee5 && -z $(git -C "$baseline_source" status --porcelain --untracked-files=all) ]] || { echo "baseline changed during run" >&2; exit 1; }
fi
phase=complete
