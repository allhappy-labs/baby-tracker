# shellcheck shell=bash
# Host discipline for Phase 5 load runs: the run lock, the idle check, the
# disk floor, the host sampler and the contamination verdict. Sourced by
# load-node.sh (and by load_node_test.go for bt_load_evaluate_host). Written
# for the stock /bin/bash 3.2 as well as a current bash: no associative
# arrays, no empty-array expansion under set -u, no `head` in a pipefail
# pipeline (SIGPIPE).
#
# The load node may be shared with other sessions. A load run therefore
# takes a lock every other run respects, refuses to start unless the machine
# is idle, and samples the host for the whole run so a foreign burst marks the
# result contaminated (Task 3's Evaluate never passes a contaminated run).
#
# Two hosts, told apart by `uname -s`:
#   - Darwin: the Mac under OrbStack (sysctl, BSD ps, orb, sw_vers). Every
#     container runs inside OrbStack's VM, which the process list shows as one
#     OrbStack process.
#   - Linux: a dedicated VM with Docker Engine (/proc, procps top and ps,
#     lscpu). Containers' processes are host processes there: they are told
#     apart by their cgroup (/proc/<pid>/cgroup) and left out of the process
#     samples, as the Mac's VM hides them; other projects' containers are
#     still sampled one by one through docker stats.
# Everything else in these scripts sticks to what BSD and GNU tools share
# (df -P, date -u +FORMAT, find -mmin, tail -n, integer or decimal sleep).

bt_load_os=$(uname -s 2>/dev/null || true)
# BT_LOAD_PROC_ROOT: /proc on Linux (tests point it at a fake one).
bt_load_proc=${BT_LOAD_PROC_ROOT:-/proc}
# Runs and the lock live under the host's tmp: /private/tmp on the Mac (/tmp
# is a symlink to it), /tmp on Linux.
if [[ $bt_load_os == Linux ]]; then bt_load_tmp=/tmp; else bt_load_tmp=/private/tmp; fi
BT_LOAD_LOCK=${BT_LOAD_LOCK:-$bt_load_tmp/bt-load.lock}
BT_LOAD_PROJECT=${BT_LOAD_PROJECT:-bt-load}
# Processes (the executable's base name) that mean another session is
# building, testing or simulating. They count when they use >= 5% CPU: an
# open but idle Simulator window is not a measurement hazard.
BT_LOAD_FOREIGN='xcodebuild|swift-build|swift-frontend|swift-test|swiftc|Simulator|go-build|compile|test-db\.sh|protocol-e2e\.sh|mcp-e2e\.sh|sync-performance\.sh|chromium|Chromium'
# Command lines that mean the same whatever their CPU at the sampled instant:
# a `go test` waiting on its package binaries (which run from go-build
# directories), an Xcode or SwiftPM test, a test-db.sh between steps, a
# Docker build (it runs inside the VM, which the process list cannot see).
BT_LOAD_FOREIGN_ARGS='(^|[ /])(xcodebuild|swift-build|swift-test|swift-frontend)( |$)|(^|[ /])swift (test|build)( |$)|(^|[ /])go (test|build|vet|run)( |$)|/go-build[0-9]+/|(test-db|protocol-e2e|mcp-e2e|sync-performance|smoke|refresh-recovery-e2e|test-performance-runner)\.sh|(^|[ /])docker( compose .*)? (build|buildx)( |$)|(^|[ /])docker-buildx|(^|[ /])make (server-test|test)( |$)'
# Processes the verdicts never count: OrbStack's VM (the measured stack runs
# in it; other projects' containers are sampled one by one instead) and the
# generator if it ever runs natively. On Linux the same role falls to the
# Docker engine (dockerd, containerd and its shims, docker-proxy, runc) and to
# the kernel's softirq and worker threads, which carry the measured stack's
# network and disk work (inside OrbStack's VM on the Mac). Linux names are
# the bare comm (no path), so they are matched whole.
BT_LOAD_IGNORED='OrbStack|com\.orbstack|vmgr|baby-tracker-load|^(dockerd|containerd|containerd-shim|containerd-shim-runc-v2|docker-proxy|runc)$|^(ksoftirqd|kworker)/'

# --- the lock ------------------------------------------------------------------
#
# A directory made atomically with mkdir. A run spans several invocations
# (preflight, up, seed, start, run, audit, down), each of which exits, so the
# lock is not tied to one process: it records the run it belongs to, the pid
# of the latest invocation and a lease (`expires`, epoch seconds) that every
# invocation of the same run renews. It is held while that pid is alive or
# the lease runs; `down` releases it. A run that never calls down blocks
# others for at most BT_LOAD_LOCK_HOURS (default 4) after its last command.

bt_load_lock_field() { cat "$BT_LOAD_LOCK/$1" 2>/dev/null || true; }

# Succeeds while the lock at $BT_LOAD_LOCK is held by someone.
bt_load_lock_live() {
  local pid expires
  pid=$(bt_load_lock_field pid)
  expires=$(bt_load_lock_field expires)
  if [[ $pid =~ ^[0-9]+$ ]] && kill -0 "$pid" 2>/dev/null; then return 0; fi
  if [[ $expires =~ ^[0-9]+$ ]] && ((expires > $(date +%s))); then return 0; fi
  # Another preflight made the directory and has not written its pid yet.
  if [[ -z $pid && -n $(find "$BT_LOAD_LOCK" -maxdepth 0 -mmin -1 2>/dev/null) ]]; then return 0; fi
  return 1
}

bt_load_stamp_lock() {
  local hours=${BT_LOAD_LOCK_HOURS:-4}
  [[ $hours =~ ^[0-9]{1,2}$ ]] || { echo "BT_LOAD_LOCK_HOURS must be 1-12" >&2; return 1; }
  hours=$((10#$hours))
  ((hours >= 1 && hours <= 12)) || { echo "BT_LOAD_LOCK_HOURS must be 1-12" >&2; return 1; }
  printf '%s\n' "$$" > "$BT_LOAD_LOCK/pid"
  printf '%s\n' "$(($(date +%s) + hours * 3600))" > "$BT_LOAD_LOCK/expires"
}

# bt_load_renew_lock RUN: renew the lease while RUN still holds the lock
# (long run and seed commands call it periodically and when they finish).
bt_load_renew_lock() {
  [[ -d $BT_LOAD_LOCK && ! -L $BT_LOAD_LOCK && $(bt_load_lock_field run) == "$1" ]] || return 1
  bt_load_stamp_lock
}

# Two preflights can both find the lock stale. The rename is atomic, so only
# one of them moves it aside; the other's mv fails and it refuses. A winner
# that finds it moved a live lock (one that changed hands after its check)
# puts it back and refuses too. Then mkdir, which again only one can win.
bt_load_take_over_stale_lock() {
  local aside
  aside="$BT_LOAD_LOCK.stale.$$.$(date +%s)"
  if ! mv "$BT_LOAD_LOCK" "$aside" 2>/dev/null; then
    echo "another load run took $BT_LOAD_LOCK" >&2
    return 1
  fi
  if BT_LOAD_LOCK=$aside bt_load_lock_live; then
    if [[ ! -e $BT_LOAD_LOCK ]]; then mv "$aside" "$BT_LOAD_LOCK" 2>/dev/null || true; fi
    echo "another load run took $BT_LOAD_LOCK" >&2
    return 1
  fi
  rm -rf "$aside"
  mkdir -m 0700 "$BT_LOAD_LOCK" 2>/dev/null || { echo "another load run took $BT_LOAD_LOCK" >&2; return 1; }
}

# bt_load_take_lock RUN: take the lock for RUN, or renew it when RUN holds it.
bt_load_take_lock() {
  local run=$1
  if [[ -L $BT_LOAD_LOCK ]]; then echo "refusing a symlinked load lock $BT_LOAD_LOCK" >&2; return 1; fi
  if ! mkdir -m 0700 "$BT_LOAD_LOCK" 2>/dev/null; then
    if [[ $(bt_load_lock_field run) == "$run" ]]; then bt_load_stamp_lock; return; fi
    if bt_load_lock_live; then
      echo "another load run holds $BT_LOAD_LOCK (pid $(bt_load_lock_field pid), run $(bt_load_lock_field run)); wait for it to finish, or end a finished run with: load-node.sh down --run <its run>" >&2
      return 1
    fi
    echo "removing stale load lock (pid $(bt_load_lock_field pid) is gone and its lease has expired; run $(bt_load_lock_field run))" >&2
    bt_load_take_over_stale_lock || return 1
  fi
  printf '%s\n' "$run" > "$BT_LOAD_LOCK/run"
  bt_load_stamp_lock
}

# bt_load_require_lock RUN: every subcommand after preflight runs only while
# RUN holds the lock, and renews its lease.
bt_load_require_lock() {
  local run=$1
  if [[ -L $BT_LOAD_LOCK || ! -d $BT_LOAD_LOCK || $(bt_load_lock_field run) != "$run" ]]; then
    echo "run $run does not hold $BT_LOAD_LOCK (held by: $(bt_load_lock_field run)); run preflight first" >&2
    return 1
  fi
  bt_load_stamp_lock
}

# bt_load_lock_free_or_ours RUN: `down` may clean up a run whose lease has
# lapsed, but never touches the stack while another run holds the lock.
bt_load_lock_free_or_ours() {
  local run=$1
  [[ -d $BT_LOAD_LOCK ]] || return 0
  [[ $(bt_load_lock_field run) == "$run" ]] && return 0
  bt_load_lock_live || return 0
  echo "run $run does not hold $BT_LOAD_LOCK (held by: $(bt_load_lock_field run)); not touching the stack" >&2
  return 1
}

bt_load_release_lock() {
  local run=$1
  if [[ -d $BT_LOAD_LOCK && ! -L $BT_LOAD_LOCK && $(bt_load_lock_field run) == "$run" ]]; then rm -rf "$BT_LOAD_LOCK"; fi
  return 0
}

# --- host samples --------------------------------------------------------------

# Shared jq definitions. command_rule names a foreign command by the rule it
# matched ("go test", "xcodebuild", "test-db.sh", "go-build", "docker build"),
# so a build's ever-changing compiler processes count as one thing across
# samples. Samples hold {rule, exe, pcpu} objects; plain strings (none are
# written any more) are still named by matching them.
# shellcheck disable=SC2016 # jq program text
bt_load_jq_defs='
  def base: split("/") | last;
  def command_rule: if type == "object" then .rule
    else . as $line | (([match($args).string] | first) // $line)
      | gsub("^[ /]+|[ /]+$"; "") | sub("^go-build[0-9]+$"; "go-build") | sub("^docker.*$"; "docker build") end;
'

# Other sessions' builds and tests, found by their command lines (never this
# runner's own: its invocation names load-node.sh, its Compose calls name its
# project). Each is printed as "rule<TAB>executable base name<TAB>%CPU" only:
# a command line can carry a credential, so none is ever recorded. On Linux a
# container's processes are skipped (their cgroup names a Docker container),
# as the Mac's VM hides them, and %CPU is GNU ps's lifetime average: it is
# recorded only, no rule reads it.
bt_load_foreign_commands() {
  local listing proc=""
  listing=$(LC_ALL=C ps -Ao pid=,pcpu=,args=) || return 1
  if [[ $bt_load_os == Linux ]]; then proc=$bt_load_proc; fi
  SELF=$$ ARGS_PAT=$BT_LOAD_FOREIGN_ARGS PROJECT=$BT_LOAD_PROJECT PROC=$proc awk '
    function in_container(pid,   file, row, hit) {
      file = ENVIRON["PROC"] "/" pid "/cgroup"
      hit = 0
      while ((getline row < file) > 0) if (row ~ /docker-[0-9a-f]+\.scope|\/docker\/[0-9a-f]+/) hit = 1
      close(file)
      return hit
    }
    { pid = $1; pcpu = $2; line = $0; sub(/^ *[0-9]+ +[^ ]+ +/, "", line) }
    pid == ENVIRON["SELF"] { next }
    index(line, "load-node.sh") || index(line, "--project-name " ENVIRON["PROJECT"] " ") { next }
    match(line, ENVIRON["ARGS_PAT"]) && !(ENVIRON["PROC"] != "" && in_container(pid)) {
      rule = substr(line, RSTART, RLENGTH)
      gsub(/^[ \/]+|[ \/]+$/, "", rule)
      sub(/^go-build[0-9]+$/, "go-build", rule)
      if (rule ~ /docker/) rule = "docker build"
      exe = line
      sub(/ .*$/, "", exe)
      n = split(exe, parts, "/")
      printf "%s\t%s\t%s\n", rule, parts[n], pcpu
    }' <<<"$listing"
}

# "name pcpu" for every running container outside this run's project. A
# container whose CPU Docker cannot report (`--` while it starts or stops)
# is printed as "name ?": recorded as unknown, never failing the sample.
bt_load_foreign_containers() {
  local stats
  stats=$(docker stats --no-stream --format '{{.Name}} {{.CPUPerc}}') || return 1
  PREFIX="$BT_LOAD_PROJECT-" awk 'NF >= 2 && index($1, ENVIRON["PREFIX"]) != 1 {
    gsub(/%/, "", $2)
    if ($2 !~ /^[0-9]+(\.[0-9]+)?$/) $2 = "?"
    print $1, $2
  }' <<<"$stats"
}

# bt_load_loadavg: the 1-, 5- and 15-minute load averages, as the host
# prints them: "{ 0.60 0.70 0.80 }" from sysctl on the Mac, "0.60 0.70 0.80"
# from /proc/loadavg on Linux.
bt_load_loadavg() {
  local one five fifteen rest
  if [[ $bt_load_os == Linux ]]; then
    read -r one five fifteen rest 2>/dev/null < "$bt_load_proc/loadavg" || return 1
    printf '%s %s %s\n' "$one" "$five" "$fifteen"
  else
    LC_ALL=C sysctl -n vm.loadavg
  fi
}

# bt_load_cpu_window: the seconds over which Linux process CPU is measured
# (BT_LOAD_CPU_WINDOW, 1-99, default 2).
bt_load_cpu_window() {
  local window=${BT_LOAD_CPU_WINDOW:-2}
  [[ $window =~ ^[1-9][0-9]?$ ]] || return 1
  printf '%s\n' "$window"
}

# bt_load_top_processes: one process snapshot, the 8 busiest as "%CPU
# command" lines. On the Mac, BSD ps: %cpu is the kernel's decaying average,
# so one read is a recent rate. On Linux, GNU ps's %cpu is the lifetime
# average, so the rate comes from top's second frame (top -b -n 2 -d WINDOW:
# the CPU each process used over the last WINDOW seconds, 100% a core, as on
# the Mac); its columns are found by the header (no user toprc is read), and
# a process in a Docker container is left out. Fails unless both frames
# parse.
bt_load_top_processes() {
  local window frames proc
  if [[ $bt_load_os != Linux ]]; then
    LC_ALL=C ps -Ao pcpu=,comm= -r | awk 'NR <= 8'
    return
  fi
  window=$(bt_load_cpu_window) || return 1
  frames=$(env -u XDG_CONFIG_HOME HOME=/nonexistent LC_ALL=C top -b -n 2 -d "$window" -w 512) || return 1
  proc=$bt_load_proc
  awk '
    $0 ~ /(^| )PID / && $0 ~ / %CPU / && $0 ~ / COMMAND/ {
      frames++; rows = ""; p = c = m = 0
      for (i = 1; i <= NF; i++) { if ($i == "PID") p = i; if ($i == "%CPU") c = i; if ($i == "COMMAND") m = i }
      next
    }
    frames && p && c && m && NF >= m && $p ~ /^[0-9]+$/ {
      command = $m
      for (i = m + 1; i <= NF; i++) command = command " " $i
      rows = rows $c "\t" $p "\t" command "\n"
    }
    END { if (frames != 2 || !p || !c || !m) exit 1; printf "%s", rows }' <<<"$frames" |
    LC_ALL=C sort -t "$(printf '\t')" -k1,1nr |
    PROC=$proc awk -F '\t' '
      function in_container(pid,   file, row, hit) {
        file = ENVIRON["PROC"] "/" pid "/cgroup"
        hit = 0
        while ((getline row < file) > 0) if (row ~ /docker-[0-9a-f]+\.scope|\/docker\/[0-9a-f]+/) hit = 1
        close(file)
        return hit
      }
      n < 8 && !in_container($2) { n++; print $1 " " $3 }'
}

# bt_load_host_sample [SNAPSHOTS GAP] prints one host sample as a JSON line:
# load averages, the top 8 processes by CPU, other sessions' build/test
# processes and other projects' containers. With SNAPSHOTS > 1 the process
# list is read SNAPSHOTS times GAP seconds apart (top_snapshots) for the
# sustained above-50% rule (on Linux each read itself spans
# BT_LOAD_CPU_WINDOW seconds). Fail-closed: any tool failing, empty output or
# a value that does not parse (a comma decimal, say) fails the sample, and
# callers treat a failed sample as "could not sample the host".
bt_load_host_sample() {
  local snapshots=${1:-1} gap=${2:-0} avg top all="" i commands containers
  [[ $snapshots =~ ^[1-9]$ && $gap =~ ^[0-9]{1,3}$ ]] || return 1
  avg=$(bt_load_loadavg) || return 1
  [[ -n $avg ]] || return 1
  for ((i = 1; i <= snapshots; i++)); do
    top=$(bt_load_top_processes) || return 1
    [[ -n $top ]] || return 1
    all="$all$top"$'\n''--'$'\n'
    if ((i < snapshots)); then sleep "$((10#$gap))"; fi
  done
  commands=$(bt_load_foreign_commands) || return 1
  containers=$(bt_load_foreign_containers) || return 1
  jq -cn --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg avg "$avg" --arg snapshots "$all" --arg commands "$commands" \
    --arg containers "$containers" '
    def lines: split("\n") | map(select(test("\\S")));
    def parsed(re): capture(re) // error("unparsed host line: \(.)");
    def procs: lines | map(parsed("^ *(?<pcpu>[0-9]+(\\.[0-9]+)?) +(?<comm>.+)$") | .pcpu |= tonumber);
    ($avg | gsub("[{}]"; "") | [splits(" +")] | map(select(length > 0))) as $l
    | if ($l | length) < 2 or ($l[0:2] | any(test("^[0-9]+(\\.[0-9]+)?$") | not)) then error("unparsed load average: \($avg)") else . end
    | ($snapshots | split("--\n") | map(select(test("\\S")) | procs)) as $tops
    | {at: $at, loadavg: $avg, load1: ($l[0] | tonumber), load5: ($l[1] | tonumber), top: $tops[0],
       foreign_commands: ($commands | lines | map(split("\t") | {rule: .[0], exe: .[1], pcpu: (.[2] | tonumber)})),
       containers: ($containers | lines | map(parsed("^(?<name>\\S+) +(?<pcpu>[0-9]+(\\.[0-9]+)?|\\?)$")
         | if .pcpu == "?" then {name, pcpu: null, cpu_unknown: true} else .pcpu |= tonumber end))}
    + (if ($tops | length) > 1 then {top_snapshots: $tops} else {} end)'
}

# bt_load_idle_once prints a JSON sample with a "reason" and returns non-zero
# with the reason on stderr when the machine is not idle, or when the host
# could not be sampled (fail-closed even when called inside `||`, where set -e
# is off). Not idle means:
#   - 1-minute load >= 3.00 or 5-minute >= 4.00 (10 cores);
#   - a foreign heavy process (BT_LOAD_FOREIGN) at >= 5% CPU;
#   - any other process, not OrbStack (on Linux: not the Docker engine, the
#     kernel's workers or a container's process), above 50% CPU in 2 or more
#     of 3 process snapshots BT_LOAD_HOT_GAP (10) seconds apart, about 20 s in
#     all (controller rulings 2026-10-16: a single-snapshot spike of a desktop
#     app does not block; macOS %cpu is a decaying average, so the snapshots
#     are 10 s apart; on Linux each snapshot is the rate over its own
#     BT_LOAD_CPU_WINDOW, so about 26 s in all);
#   - another session's build or test command (BT_LOAD_FOREIGN_ARGS);
#   - another project's container above 25% CPU.
bt_load_idle_once() {
  local sample reason gap=${BT_LOAD_HOT_GAP:-10}
  if ! sample=$(bt_load_host_sample 3 "$gap") || [[ -z $sample ]]; then
    echo "could not sample the host" >&2
    return 1
  fi
  if ! reason=$(jq -r --arg foreign "$BT_LOAD_FOREIGN" --arg ignored "$BT_LOAD_IGNORED" --arg args "$BT_LOAD_FOREIGN_ARGS" "$bt_load_jq_defs"'
    (.loadavg | gsub("[{}]"; "") | [splits(" +")] | map(select(length > 0))) as $l
    | ([.foreign_commands[] | command_rule] | unique) as $commands
    | (.top_snapshots // [.top]) as $snaps
    | ([$snaps[] | [.[] | select(.pcpu > 50 and (.comm | test($ignored) | not)) | .comm | base] | unique] | add // []
       | group_by(.) | map(select(length >= ([2, ($snaps | length)] | min)) | {name: .[0], hits: length})) as $hot
    | [ (if ($l[0] | tonumber) >= 3 then "load average \($l[0]) >= 3.00" else empty end),
        (if ($l[1] | tonumber) >= 4 then "5-minute load average \($l[1]) >= 4.00" else empty end),
        ([.top[] | select(.pcpu >= 5 and (.comm | base | test("^(" + $foreign + ")$"))) | .comm | base] | unique[]
          | "machine not idle: \(.)"),
        ($hot[] | "machine not idle: \(.name) above 50% CPU in \(.hits) of \($snaps | length) samples"),
        (if ($commands | length) > 0
         then "machine not idle: running \($commands | join(", ")) (\(.foreign_commands | length) processes)" else empty end),
        (.containers[] | select(.pcpu != null and .pcpu > 25) | "machine not idle: container \(.name) at \(.pcpu)% CPU")
      ] | join("; ")' <<<"$sample"); then
    echo "could not sample the host" >&2
    return 1
  fi
  jq -c --arg reason "$reason" '. + {reason: $reason}' <<<"$sample" || { echo "could not sample the host" >&2; return 1; }
  if [[ -n $reason ]]; then echo "$reason" >&2; return 1; fi
}

# bt_load_idle_check OUT: BT_LOAD_IDLE_SAMPLES (3) samples
# BT_LOAD_IDLE_INTERVAL (20) seconds apart must all pass. Samples go to OUT as
# JSON lines; other projects' containers (running but idle) are noted and
# left in bt_load_others.
bt_load_others=""
bt_load_idle_check() {
  local out=$1 n=${BT_LOAD_IDLE_SAMPLES:-3} gap=${BT_LOAD_IDLE_INTERVAL:-20} i others
  [[ $n =~ ^[0-9]{1,2}$ && $gap =~ ^[0-9]{1,3}$ ]] || { echo "invalid idle sampling" >&2; return 1; }
  n=$((10#$n))
  gap=$((10#$gap))
  ((n >= 1)) || { echo "invalid idle sampling" >&2; return 1; }
  : > "$out"
  for ((i = 1; i <= n; i++)); do
    bt_load_idle_once >> "$out" || return 1
    if ((i < n)); then sleep "$gap"; fi
  done
  others=$(docker ps --format '{{.Names}}' 2>/dev/null | PREFIX="$BT_LOAD_PROJECT-" awk 'NF && index($0, ENVIRON["PREFIX"]) != 1' || true)
  if [[ -n $others ]]; then echo "note: other containers running: $(tr '\n' ' ' <<<"$others")" >&2; fi
  # shellcheck disable=SC2034 # read by load-node.sh preflight
  bt_load_others=$others
}

# bt_load_disk_check FAMILIES: 10 GB free for ordinary runs, 30 GB for any
# run of 1,000 families or more. Prints the free figure in whole GiB, from
# POSIX df (-P, 1024-byte blocks: BSD's -g has no GNU twin).
bt_load_disk_check() {
  local families=$1 need=10 avail
  [[ $families =~ ^[0-9]{1,9}$ ]] || { echo "invalid family count" >&2; return 1; }
  ((10#$families >= 1000)) && need=30
  avail=$(LC_ALL=C df -Pk / | awk 'NR == 2 && $4 ~ /^[0-9]+$/ { printf "%d\n", $4 / 1048576 }')
  [[ $avail =~ ^[0-9]+$ ]] || { echo "could not read free disk from df" >&2; return 1; }
  printf '%s %s\n' "$avail" "$need"
  ((10#$avail >= need)) || { echo "free disk $avail GB < $need GB" >&2; return 1; }
}

# --- host facts and the node label ----------------------------------------------

# bt_load_host_facts prints the host's facts as one JSON object. It never
# fails: a fact that cannot be read is null.
#   Darwin: chip (machdep.cpu.brand_string), host_ncpu, host_memsize, macos.
#   Linux: cpu_model, host_ncpu (logical CPUs), host_sockets, host_cores,
#     host_threads_per_core, host_memsize (MemTotal), kernel, os_release
#     (BT_LOAD_OS_RELEASE, default /etc/os-release), hypervisor_vendor and
#     virtualization (lscpu), hypervisor_flag (cpuinfo's "hypervisor" flag).
#     lscpu is preferred; /proc/cpuinfo fills what it does not print.
bt_load_host_facts() {
  local hw="" macos="" cpu="" lscpu="" mem="" kernel="" release="" keyvalue
  if [[ $bt_load_os != Linux ]]; then
    hw=$(LC_ALL=C sysctl -n hw.memsize hw.ncpu machdep.cpu.brand_string 2>/dev/null | tr '\n' '|' || true)
    macos=$(sw_vers -productVersion 2>/dev/null || true)
    jq -cn --arg hw "$hw" --arg macos "$macos" '
      def num: if . == null or . == "" then null else (tonumber? // null) end;
      ($hw | split("|")) as $h
      | {host_os: "Darwin", host_memsize: ($h[0] | num), host_ncpu: ($h[1] | num),
         chip: (($h[2] // "") | if . == "" then null else . end), macos: $macos}'
    return 0
  fi
  # Both read as key and value: lscpu indents its sections (util-linux 2.38
  # and later) and cpuinfo pads its keys with tabs.
  # shellcheck disable=SC2016 # awk program text
  keyvalue='{ key = $0; sub(/[ \t]*:.*$/, "", key); sub(/^[ \t]+/, "", key); value = $0
    if (!sub(/^[^:]*:[ \t]*/, "", value)) next; sub(/[ \t]+$/, "", value) }'
  cpu=$(LC_ALL=C awk "$keyvalue"'
    key == "processor" { n++ }
    key == "model name" && model == "" { model = value }
    key == "cpu cores" && cores == "" { cores = value }
    key == "siblings" && siblings == "" { siblings = value }
    key == "physical id" && !(value in ids) { ids[value] = 1; sockets++ }
    key == "flags" && (" " value " ") ~ / hypervisor / { hv = 1 }
    END { printf "%s\t%s\t%s\t%s\t%s\t%s\n", model, n, cores, siblings, sockets, hv }' "$bt_load_proc/cpuinfo" 2>/dev/null) || cpu=""
  lscpu=$(LC_ALL=C lscpu 2>/dev/null | LC_ALL=C awk "$keyvalue"'
    key == "Model name" && model == "" { model = value }
    key == "CPU(s)" && n == "" { n = value }
    key == "Socket(s)" && sockets == "" { sockets = value }
    key == "Core(s) per socket" && cores == "" { cores = value }
    key == "Thread(s) per core" && threads == "" { threads = value }
    key == "Hypervisor vendor" && vendor == "" { vendor = value }
    key == "Virtualization type" && virt == "" { virt = value }
    END { printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", model, n, sockets, cores, threads, vendor, virt }') || lscpu=""
  mem=$(LC_ALL=C awk '$1 == "MemTotal:" && $2 ~ /^[0-9]+$/ { printf "%.0f\n", $2 * 1024; exit }' "$bt_load_proc/meminfo" 2>/dev/null) || mem=""
  kernel=$(uname -r 2>/dev/null) || kernel=""
  release=$(awk '/^PRETTY_NAME=/ { value = substr($0, 13); gsub(/^"|"$/, "", value); print value; exit }' \
    "${BT_LOAD_OS_RELEASE:-/etc/os-release}" 2>/dev/null) || release=""
  jq -cn --arg cpu "$cpu" --arg lscpu "$lscpu" --arg mem "$mem" --arg kernel "$kernel" --arg release "$release" '
    def num: if . == null or . == "" then null else (tonumber? // null) end;
    def text: if . == null or . == "" then null else . end;
    ($cpu | split("\t")) as $c | ($lscpu | split("\t")) as $l
    | (($l[2] | num) // ($c[4] | num) // (if ($c[1] | num) != null then 1 else null end)) as $sockets
    | (($l[3] | num) // ($c[2] | num)) as $per_socket
    | {host_os: "Linux", cpu_model: (($l[0] | text) // ($c[0] | text)), host_ncpu: (($l[1] | num) // ($c[1] | num)),
       host_sockets: $sockets, host_cores: (if $per_socket != null and $sockets != null then $per_socket * $sockets else null end),
       host_threads_per_core: (($l[4] | num)
         // (if ($c[3] | num) != null and ($c[2] | num) != null then ($c[3] | num) / ($c[2] | num) else null end)),
       host_memsize: ($mem | num), kernel: ($kernel | text), os_release: ($release | text),
       hypervisor_vendor: ($l[5] | text), virtualization: ($l[6] | text), hypervisor_flag: ($c[5] == "1")}'
}

# The label the results carry, from the host facts:
#   Darwin: "<chip>, Docker CPU caps" ("Apple M4, Docker CPU caps").
#   Linux: "<CPU model> (<hypervisor> VM, <n> vCPU, <n> GiB), Docker CPU
#     caps": the model without its "(R)"/"(TM)" marks and its "8-Core
#     Processor" or "CPU @ 2.80GHz" tail; the memory is MemTotal rounded up
#     to a whole GiB (the kernel keeps a few percent of a 16 GiB VM); "CPU",
#     not "vCPU", on a host that is no VM.
# shellcheck disable=SC2016 # jq program text
bt_load_label_jq='
  def clean: gsub("\\((R|TM|tm)\\)"; "") | sub(" CPU @.*$"; "") | sub(" [0-9]+-Core Processor$"; "") | sub(" Processor$"; "")
    | gsub("  +"; " ") | sub("^ +"; "") | sub(" +$"; "");
  if .host_os == "Linux" then
    ((.cpu_model // "" | clean) | if . == "" then "unknown CPU" else . end) as $model
    | ((.hypervisor_vendor // "") as $v | {"Microsoft": "Hyper-V", "KVM": "KVM", "VMware": "VMware", "Xen": "Xen",
        "Oracle": "VirtualBox", "Parallels": "Parallels", "QEMU": "QEMU"}[$v] // $v) as $hv
    | (if $hv != "" then "\($hv) VM" elif .hypervisor_flag == true then "VM" else null end) as $vm
    | ([$vm,
        (if .host_ncpu != null then "\(.host_ncpu) \(if $vm != null then "vCPU" else "CPU" end)" else null end),
        (if .host_memsize != null then "\(.host_memsize / 1073741824 | ceil) GiB" else null end)]
       | map(select(. != null)) | join(", ")) as $inner
    | "\($model)\(if $inner == "" then "" else " (\($inner))" end), Docker CPU caps"
  else "\(.chip // "unknown Mac"), Docker CPU caps" end'

# bt_load_node_label [FACTS]: the node label, BT_LOAD_NODE_LABEL when set
# (at most 200 printable characters), else computed from FACTS
# (bt_load_host_facts when none are given).
bt_load_node_label() {
  local facts=${1:-}
  if [[ -n ${BT_LOAD_NODE_LABEL:-} ]]; then
    if [[ ${#BT_LOAD_NODE_LABEL} -gt 200 || $BT_LOAD_NODE_LABEL =~ [^[:print:]] ]]; then
      echo "BT_LOAD_NODE_LABEL must be at most 200 printable characters" >&2
      return 1
    fi
    printf '%s\n' "$BT_LOAD_NODE_LABEL"
    return 0
  fi
  [[ -n $facts ]] || facts=$(bt_load_host_facts)
  jq -r "$bt_load_label_jq" <<<"$facts"
}

# bt_load_vm_facts FACTS: the host facts plus what the containers run on:
# Docker's NCPU, MemTotal, cgroup version and driver and engine version, and
# on the Mac OrbStack's cpu and memory_mib. memory_limit_bytes is the memory
# the two capped containers share (OrbStack's memory_mib on the Mac, MemTotal
# on Linux); caps_are_ceilings says the declared 2 x 8 GiB exceed it
# (decision D2 B: the caps are then ceilings, and every run reports its
# measured peaks beside them).
bt_load_vm_facts() {
  local facts=$1 docker_info orb=""
  docker_info=$(docker info --format '{{.NCPU}} {{.MemTotal}} {{.CgroupVersion}} {{.CgroupDriver}} {{.ServerVersion}}' 2>/dev/null || true)
  if [[ $bt_load_os != Linux ]] && command -v orb >/dev/null; then
    orb=$(orb config show 2>/dev/null | awk -F': *' '$1 == "cpu" { c = $2 } $1 == "memory_mib" { m = $2 } END { print c, m }' || true)
  fi
  jq -c --arg docker "$docker_info" --arg orb "$orb" '
    def num: if . == null or . == "" then null else (tonumber? // null) end;
    def text: if . == null or . == "" then null else . end;
    ($docker | split(" ")) as $d | ($orb | split(" ")) as $o
    | . + {docker_ncpu: ($d[0] | num), docker_mem_total: ($d[1] | num), docker_cgroup_version: ($d[2] | text),
           docker_cgroup_driver: ($d[3] | text), docker_server_version: ($d[4] | text)}
    + (if .host_os == "Linux" then {memory_limit_bytes: .host_memsize, memory_limit_source: "MemTotal"}
       else {orb_cpu: ($o[0] | num), orb_memory_mib: ($o[1] | num),
             memory_limit_bytes: (($o[1] | num) as $m | if $m == null then null else $m * 1048576 end),
             memory_limit_source: "OrbStack memory_mib"} end)
    + {declared_caps_bytes: (2 * 8589934592)}
    | .caps_are_ceilings = (if .memory_limit_bytes == null then null else .memory_limit_bytes < .declared_caps_bytes end)' <<<"$facts"
}

# bt_load_host_sampler OUT ALIVE: every BT_LOAD_HOST_INTERVAL (10) seconds a
# host sample, until the file ALIVE disappears.
# On Linux a sample itself spans BT_LOAD_CPU_WINDOW seconds (top's two
# frames), which the pause makes up for, so samples stay INTERVAL apart.
bt_load_host_sampler() {
  local out=$1 alive=$2 pause=${BT_LOAD_HOST_INTERVAL:-10} window
  if [[ $bt_load_os == Linux && $pause =~ ^[0-9]{1,4}$ ]] && window=$(bt_load_cpu_window); then
    pause=$((10#$pause - window))
    ((pause >= 0)) || pause=0
  fi
  while [[ -e $alive ]] && kill -0 "$$" 2>/dev/null; do
    bt_load_host_sample >> "$out" || true
    sleep "$pause"
  done
}

# bt_load_evaluate_host SAMPLES prints {"contaminated":bool,"offenders":[…],
# "details":[…],"max_load1":x,"samples":n}. Ignoring OrbStack, the VM and the
# generator, it groups the samples by process (base name) and by foreign
# container, and flags the run contaminated when
#   - any of them averages > 50% CPU over 6 consecutive samples (60 s),
#   - a BT_LOAD_FOREIGN process at >= 5% CPU appears in >= 2 samples, or
#   - the same foreign build/test rule (command_rule) appears in >= 2
#     samples; details name one executable (base name only) as an example.
bt_load_evaluate_host() {
  jq -cs --arg foreign "$BT_LOAD_FOREIGN" --arg ignored "$BT_LOAD_IGNORED" --arg args "$BT_LOAD_FOREIGN_ARGS" "$bt_load_jq_defs"'
    def load1: if .load1 != null then .load1
      else (.loadavg // "" | gsub("[{}]"; "") | [splits(" +")] | map(select(length > 0)) | (.[0] // "0") | tonumber) end;
    . as $s | length as $n
    | ([range(0; $n) as $i | ($s[$i].top // [])[] | select(.comm | test($ignored) | not)
          | {i: $i, name: (.comm | base), pcpu: (.pcpu | tonumber), process: true}]
       + [range(0; $n) as $i | ($s[$i].containers // [])[] | select(.pcpu != null)
          | {i: $i, name: ("container " + .name), pcpu: (.pcpu | tonumber), process: false}]) as $rows
    | ($rows | group_by(.name) | map(. as $g | {name: $g[0].name, process: $g[0].process,
          series: [range(0; $n) as $i | ([$g[] | select(.i == $i) | .pcpu] | add // 0)]})) as $groups
    | ([$groups[] | select([range(0; $n - 5) as $k | (.series[$k:$k + 6] | add) / 6] | any(. > 50))
          | {name, reason: "above 50% CPU on average over 60 s"}]
       + [$groups[] | select(.process and (.name | test("^(" + $foreign + ")$")) and ([.series[] | select(. >= 5)] | length) >= 2)
          | {name, reason: "foreign build/test process in 2 or more samples"}]
       + ([range(0; $n) as $i | ($s[$i].foreign_commands // [])[]
            | {i: $i, name: command_rule, example: (if type == "object" then .exe else null end)}]
          | group_by(.name)
          | map(select((map(.i) | unique | length) >= 2)
                | {name: .[0].name, reason: "foreign build/test command in 2 or more samples", example: .[0].example}))
      ) as $details
    # No samples at all proves nothing about the host: never clean.
    | (if $n == 0 then [{name: "no host samples", reason: "the host sampler recorded nothing"}] else $details end) as $details
    | {contaminated: ($details | length > 0), offenders: ($details | map(.name) | unique), details: $details,
       max_load1: ([$s[] | load1] | max // 0), samples: $n}' "$1"
}
