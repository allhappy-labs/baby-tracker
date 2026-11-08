# shellcheck shell=bash
# Sampler loops and PostgreSQL captures for Phase 5 load runs. Sourced by
# load-node.sh. Every loop runs until its sentinel file disappears and never
# fails the run: a missed sample is a gap in the file, not an abort.

# Docker stats every BT_LOAD_DOCKER_INTERVAL (5) seconds for every container
# of the run's Compose project (app, db, and the generator's `compose run`
# container, whatever its generated name), one JSON line per container.
bt_load_docker_sampler() {
  local out=$1 alive=$2 project=$3 ids
  while [[ -e $alive ]] && kill -0 "$$" 2>/dev/null; do
    ids=$(docker ps -q --filter "label=com.docker.compose.project=$project" 2>/dev/null || true)
    if [[ -n $ids ]]; then
      # shellcheck disable=SC2086 # one argument per container ID
      docker stats --no-stream --format '{{json .}}' $ids 2>/dev/null |
        jq -c --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '{at: $at} + .' >> "$out" 2>/dev/null || true
    fi
    sleep "${BT_LOAD_DOCKER_INTERVAL:-5}"
  done
}

# Blocked lock requests, one JSON line per sample (jsonb: never a newline).
# waiting_ms is the waiter's time in its current state (since state_change);
# lock_wait_ms is the lock wait itself (pg_locks.waitstart, null in the
# instant before the waiter records it). blocker is the first pid
# pg_blocking_pids names, as pg_stat_activity shows it: its state, the age of
# its transaction and of its current statement, its wait and its statement's
# text (truncated; literals only as pg_stat_activity shows them on this
# disposable load database). Null when the blocker has already gone.
BT_LOAD_PG_LOCKS_SQL="SELECT jsonb_build_object('at', clock_timestamp(), 'blocked', coalesce(jsonb_agg(jsonb_build_object(
  'pid', l.pid, 'locktype', l.locktype, 'mode', l.mode, 'relation', l.relation::regclass::text,
  'blocked_by', bp.pids, 'usename', a.usename, 'state', a.state,
  'wait_event_type', a.wait_event_type, 'wait_event', a.wait_event,
  'waiting_ms', round(extract(epoch FROM clock_timestamp() - a.state_change) * 1000),
  'lock_wait_ms', round(extract(epoch FROM clock_timestamp() - l.waitstart) * 1000),
  'query', left(a.query, 300),
  'blocker', CASE WHEN b.pid IS NOT NULL THEN jsonb_build_object('pid', b.pid, 'usename', b.usename,
    'application_name', b.application_name, 'state', b.state, 'wait_event_type', b.wait_event_type, 'wait_event', b.wait_event,
    'xact_start', b.xact_start, 'xact_ms', round(extract(epoch FROM clock_timestamp() - b.xact_start) * 1000),
    'query_ms', round(extract(epoch FROM clock_timestamp() - b.query_start) * 1000),
    'query', left(b.query, 300)) END)), '[]'::jsonb))
FROM pg_locks l JOIN pg_stat_activity a USING (pid)
CROSS JOIN LATERAL (SELECT pg_blocking_pids(l.pid) AS pids) bp
LEFT JOIN pg_stat_activity b ON b.pid = bp.pids[1]
WHERE NOT l.granted"

# Wait-event counts of non-idle backends; a NULL wait event is a backend on
# CPU.
BT_LOAD_PG_WAITS_SQL="SELECT jsonb_build_object('at', clock_timestamp(), 'waits', coalesce(jsonb_agg(jsonb_build_object(
  'type', w.wait_event_type, 'event', w.wait_event, 'count', w.n) ORDER BY w.n DESC), '[]'::jsonb))
FROM (SELECT coalesce(wait_event_type, 'CPU') AS wait_event_type, coalesce(wait_event, 'running') AS wait_event, count(*) AS n
      FROM pg_stat_activity WHERE state <> 'idle' AND pid <> pg_backend_pid() GROUP BY 1, 2) w"

# PostgreSQL every BT_LOAD_PG_INTERVAL (1) second through one psql call:
# blocked locks to LOCKS_OUT, wait events to WAITS_OUT.
bt_load_pg_sampler() {
  local db=$1 locks_out=$2 waits_out=$3 alive=$4 rows
  while [[ -e $alive ]] && kill -0 "$$" 2>/dev/null; do
    rows=$(docker exec "$db" psql -XAt -U postgres -d babytracker -c "$BT_LOAD_PG_LOCKS_SQL" -c "$BT_LOAD_PG_WAITS_SQL" 2>/dev/null || true)
    if [[ -n $rows ]]; then
      awk 'NR == 1' <<<"$rows" >> "$locks_out"
      awk 'NR == 2' <<<"$rows" >> "$waits_out"
    fi
    sleep "${BT_LOAD_PG_INTERVAL:-1}"
  done
}

# The consumed-session backlog the retention sweep owes (Task 17): sessions
# consumed more than 24 hours ago, with the query's own duration. The count
# is an index-only range scan of sessions_consumed_idx (consumed_at, partial
# on status = 'consumed'); a 10 s statement timeout bounds a slow sample,
# which is then a gap, not a stall, and the scan stays serial (one of the
# database's capped cores). Its statement, like the pg sampler's, appears in
# pg_stat_statements under the postgres user.
BT_LOAD_BACKLOG_SQL="WITH t0 AS MATERIALIZED (SELECT clock_timestamp() AS t0),
c AS MATERIALIZED (SELECT count(*) AS n FROM t0, sessions WHERE status = 'consumed' AND consumed_at < now() - interval '24 hours')
SELECT jsonb_build_object('at', t0.t0, 'due', c.n, 'ms', round((extract(epoch FROM clock_timestamp() - t0.t0) * 1000)::numeric, 1)) FROM t0, c"

# The backlog every BT_LOAD_BACKLOG_INTERVAL (60) seconds into OUT, one JSON
# line per sample.
bt_load_backlog_sampler() {
  local db=$1 out=$2 alive=$3 row
  while [[ -e $alive ]] && kill -0 "$$" 2>/dev/null; do
    row=$(docker exec "$db" psql -XAtq -U postgres -d babytracker -c "SET statement_timeout = '10s'" \
      -c "SET max_parallel_workers_per_gather = 0" -c "$BT_LOAD_BACKLOG_SQL" 2>/dev/null || true)
    if [[ -n $row ]]; then printf '%s\n' "$row" >> "$out"; fi
    bt_load_sleep_while "$alive" "${BT_LOAD_BACKLOG_INTERVAL:-60}"
  done
}

# bt_load_sleep_while SENTINEL SECONDS: sleep up to SECONDS, a second at a
# time, returning early once SENTINEL is gone (a minute-long sampler must
# not hold the end of a run).
bt_load_sleep_while() {
  local alive=$1 n=0 seconds=$2
  [[ $seconds =~ ^[0-9]{1,5}$ ]] || seconds=60
  seconds=$((10#$seconds))
  while ((n < seconds)) && [[ -e $alive ]] && kill -0 "$$" 2>/dev/null; do
    sleep 1
    n=$((n + 1))
  done
}

# bt_load_apns_records BODY LAST AT EPOCH: the identitytest APNs recorder's
# records (GET /__identitytest/apns) after sequence LAST, one compact JSON
# line each, stamped with the sample's time AT and the recorder's EPOCH
# (a restarted app starts a new recorder). received_at is the recorder's
# own time for the request (absent from an older image).
bt_load_apns_records() {
  jq -c --argjson last "$2" --arg at "$3" --argjson epoch "$4" '.requests // [] | .[] | select(.sequence > $last)
    | {sampled_at: $at, recorder_epoch: $epoch, sequence, received_at, token, push_type: .headers["apns-push-type"],
       topic: .headers["apns-topic"], apns_id: .headers["apns-id"], priority: .headers["apns-priority"], acknowledged}' "$1"
}

# BT_LOAD_APNS_DUPLICATE_SECONDS: two pushes of one push type to one token
# received less than this apart are a duplicate: wake.APNsInterval (20
# minutes, internal/wake/push.go: at most one background push per device
# every 20 minutes) less 10 s for the wake tick's own spread.
BT_LOAD_APNS_DUPLICATE_SECONDS=1190

# bt_load_apns_summary RECORDS SAMPLES: the run's APNs records and samples:
# the records by push type with the duplicates per token (two pushes of the
# type received less than BT_LOAD_APNS_DUPLICATE_SECONDS apart, by the
# recorder's received_at; Task 18's R3 signal for wakes, "background"); a
# Live Activity's updates repeat by design and are reported as
# by_design_repeats, not duplicates. From the samples: the records the
# sampler never saw (lost: the ring of 4,096 wrapped between two samples),
# the coverage, the recorder epochs (app restarts) and the in-flight peak.
bt_load_apns_summary() {
  jq -n --slurpfile records "$1" --slurpfile samples "$2" --argjson window "$BT_LOAD_APNS_DUPLICATE_SECONDS" '
    def ts: capture("^(?<b>[0-9T:-]+)(?<f>\\.[0-9]+)?Z$") | ((.b + "Z") | fromdateiso8601) + (("0" + (.f // ".0")) | tonumber);
    ($records | map(select(.sequence != null))) as $r
    | ($samples | map(select(.total != null))) as $s
    | ($s | map(.lost) | add // 0) as $lost
    | ($s | map(select(.restarted == true)) | length) as $restarts
    | {records: ($r | length), samples: ($s | length), lost: $lost,
       coverage: (if ($r | length) + $lost == 0 then null else (($r | length) / (($r | length) + $lost)) end),
       epochs: ([$s[].recorder_epoch] | unique | length), restarts: $restarts,
       lost_unknown_at_restart: ($restarts > 0),
       in_flight_peak: ([$s[].in_flight_peak] | max // 0),
       in_flight_peak_by_epoch: ($s | group_by(.recorder_epoch) | map({key: (.[0].recorder_epoch | tostring), value: (map(.in_flight_peak) | max)}) | from_entries),
       records_without_received_at: ($r | map(select(.received_at == null)) | length),
       duplicate_window_seconds: $window,
       by_push_type: ($r | group_by(.push_type) | map(.[0].push_type as $type | {key: ($type // "unknown"), value: (
         map(select(.received_at != null)) | group_by(.token) as $tokens
         | ($tokens | map(map(.received_at | ts) | sort | [range(1; length) as $i | .[$i] - .[$i - 1] | select(. < $window)] | length)) as $close
         | {records: length, tokens: ($tokens | length), max_per_token: ($tokens | map(length) | max // 0)}
           + (if $type == "liveactivity"
              then {by_design_repeats: ($close | add // 0), tokens_with_repeats: ($close | map(select(. > 0)) | length)}
              else {duplicates: ($close | add // 0), tokens_with_duplicates: ($close | map(select(. > 0)) | length)} end))}) | from_entries),
       peak_note: "in_flight_peak is the recorder'"'"'s lifetime peak since the app started (warm-up included): conservative for Task 18 Step 5'"'"'s > 100 rule",
       note: ("a duplicate is two pushes of one push type to one token received (the recorder'"'"'s received_at) less than \($window) s apart (wake.APNsInterval less 10 s); Live Activity updates repeat by design; a record'"'"'s acknowledged flag is read when it is sampled, so a request still inside the sink'"'"'s answer delay then may read acknowledged and later be lost (optimistic)"
         + (if $lost > 0 then "; \($lost) records were lost between samples (the 4,096 ring wrapped): the duplicate check is partial, lower BT_LOAD_APNS_INTERVAL" else "" end)
         + (if $restarts > 0 then "; the app restarted \($restarts) time(s): what each old recorder took after its last sample is unknown" else "" end))}'
}

# The extension lives in the `postgres` maintenance database, so the
# babytracker catalog and the readiness privilege audit are untouched; its
# view still covers every database.
bt_load_pg_statements_reset() {
  docker exec "$1" psql -XAtq -U postgres -d postgres -c 'SELECT pg_stat_statements_reset()' >/dev/null
}

# The top 50 babytracker statements by total execution time, with what
# PostgreSQL 17's pg_stat_statements 1.11 adds to the time: planning (plans
# and the plan times, zero unless pg_stat_statements.track_planning is on,
# as the load overlay sets it), the exec-time spread, the time spent reading
# and writing shared blocks (track_io_timing), the blocks dirtied and
# written, and the WAL each statement generated (records, full-page images,
# bytes). The dump's settings record the timing settings the db ran with
# (track_wal_io_timing is the cluster snapshots' write and sync times).
BT_LOAD_PG_STATEMENTS_COLUMNS="queryid::text AS queryid, calls, total_exec_time, mean_exec_time, min_exec_time, max_exec_time,
  stddev_exec_time, plans, total_plan_time, mean_plan_time, max_plan_time, rows, shared_blks_hit, shared_blks_read,
  shared_blks_dirtied, shared_blks_written, shared_blk_read_time, shared_blk_write_time, temp_blks_written,
  wal_records, wal_fpi, wal_bytes, query"

bt_load_pg_statements_dump() {
  local db=$1 out=$2 window_start=$3
  docker exec "$db" psql -XAt -U postgres -d postgres -c "SELECT jsonb_build_object('settings', jsonb_build_object(
      'track_planning', current_setting('pg_stat_statements.track_planning', true), 'track_io_timing', current_setting('track_io_timing'),
      'track_wal_io_timing', current_setting('track_wal_io_timing')),
    'top', (SELECT coalesce(jsonb_agg(r ORDER BY r.total_exec_time DESC), '[]'::jsonb)
      FROM (SELECT $BT_LOAD_PG_STATEMENTS_COLUMNS
            FROM pg_stat_statements WHERE dbid = (SELECT oid FROM pg_database WHERE datname = 'babytracker')
            ORDER BY total_exec_time DESC LIMIT 50) r))" |
    jq -c --arg window_start "$window_start" --arg dumped_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      '{window_start: $window_start, dumped_at: $dumped_at, settings: .settings, top_by_total_exec_time: .top}' > "$out"
}

# The cluster's WAL, checkpointer and background-writer counters, one JSON
# line per snapshot. pg_stat_wal's wal_buffers_full counts WAL writes forced
# by full WAL buffers, wal_write and wal_sync the writes and syncs; their
# _time columns stay zero unless track_wal_io_timing is on (settings records
# it). The counters are cumulative, and each backend flushes its share about
# once a second, so a window's figure is its end snapshot less its start.
BT_LOAD_PG_CLUSTER_SQL="SELECT jsonb_build_object('at', clock_timestamp(), 'epoch', extract(epoch FROM clock_timestamp()),
  'settings', jsonb_build_object('track_wal_io_timing', current_setting('track_wal_io_timing'),
    'wal_buffers', current_setting('wal_buffers'), 'wal_compression', current_setting('wal_compression'),
    'synchronous_commit', current_setting('synchronous_commit'), 'commit_delay', current_setting('commit_delay'),
    'checkpoint_timeout', current_setting('checkpoint_timeout'), 'max_wal_size', current_setting('max_wal_size')),
  'wal', (SELECT to_jsonb(w) FROM pg_stat_wal w),
  'checkpointer', (SELECT to_jsonb(c) FROM pg_stat_checkpointer c),
  'bgwriter', (SELECT to_jsonb(b) FROM pg_stat_bgwriter b))"

# bt_load_pg_cluster_snapshot DB OUT PHASE: append the snapshot, labelled
# PHASE (window_start or window_end), to OUT. A window_end snapshot also
# carries delta: each numeric counter less the last window_start line's, and
# the seconds between the two. A failed snapshot is a missing line, never an
# abort.
bt_load_pg_cluster_snapshot() {
  local db=$1 out=$2 phase=$3 row start=""
  row=$(docker exec "$db" psql -XAt -U postgres -d postgres -c "$BT_LOAD_PG_CLUSTER_SQL" 2>/dev/null) || return 0
  [[ -n $row ]] || return 0
  if [[ $phase == window_end && -s $out ]]; then
    start=$(jq -c 'select(.phase == "window_start")' "$out" 2>/dev/null | tail -n 1) || start=""
  fi
  jq -c --arg phase "$phase" --arg start "$start" '
    def counters($a; $b): $b | with_entries(select((.value | type) == "number" and (($a[.key] // null) | type) == "number")
      | .value -= $a[.key]);
    {phase: $phase} + .
    | if $phase == "window_end" and $start != "" then
        ($start | fromjson) as $s
        | . + {delta: {seconds: (.epoch - $s.epoch), wal: counters($s.wal; .wal),
                       checkpointer: counters($s.checkpointer; .checkpointer), bgwriter: counters($s.bgwriter; .bgwriter)}}
      else . end' <<<"$row" >> "$out" 2>/dev/null || true
}

# bt_load_resources STATS CAPS [LABEL]: mean and peak CPU and peak memory per
# service from docker-stats.jsonl, printed next to the caps and the VM's
# memory (decision D2: OrbStack stays at 12 GiB, so the two 8 GiB caps are
# ceilings that cannot both be reached; on a Linux VM the same holds against
# its MemTotal when that is under 16 GiB; every run reports its measured
# peaks). LABEL is the node's label (else caps.json's).
bt_load_resources() {
  local stats=$1 caps=$2 label=${3:-}
  jq -s --slurpfile caps "$caps" --arg label "$label" '
    def bytes: capture("^(?<n>[0-9.]+)\\s*(?<u>[A-Za-z]*)") | (.n | tonumber) *
      ({"B": 1, "KiB": 1024, "MiB": 1048576, "GiB": 1073741824, "TiB": 1099511627776, "kB": 1000, "KB": 1000, "MB": 1e6, "GB": 1e9, "TB": 1e12}[.u] // 1);
    def service: if test("-loadgen-") then "loadgen" elif test("-app-") then "app" elif test("-db-") then "db" else "other" end;
    def pct: rtrimstr("%") | tonumber;
    ($caps[0] // {}) as $c
    | (map(select(.Name != null) | {service: (.Name | service), cpu: (.CPUPerc | pct), mem: (.MemUsage | split(" / ")[0] | bytes)})
       | group_by(.service)
       | map({key: .[0].service, value: {samples: length, cpu_mean_pct: ((map(.cpu) | add) / length * 100 | round / 100),
              cpu_peak_pct: (map(.cpu) | max), mem_peak_bytes: (map(.mem) | max | floor)}}) | from_entries) as $by
    | ($c.vm // {}) as $vm
    | {label: (if $label != "" then $label else ($c.label // "Apple M4, Docker CPU caps") end),
       vm: $vm,
       caps: {app: {cpus: 4, memory_bytes: 8589934592}, db: {cpus: 4, memory_bytes: 8589934592}, loadgen: "uncapped"},
       measured: $by,
       note: (if $vm.host_os == "Linux" then
           if $vm.caps_are_ceilings == false
           then "The caps fit: app 8 GiB + db 8 GiB are within the Linux VM'"'"'s MemTotal stated in vm (memory_limit_bytes); measured peaks are reported beside it."
           else "The caps are ceilings: app 8 GiB + db 8 GiB exceed the Linux VM'"'"'s MemTotal stated in vm (memory_limit_bytes; decision D2 B); measured peaks are reported beside it." end
         else "The caps are ceilings: app 8 GiB + db 8 GiB exceed the OrbStack VM memory stated in vm (decision D2); measured peaks are reported beside it." end)}' "$stats"
}
