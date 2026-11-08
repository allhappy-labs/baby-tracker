# PostgreSQL authority Phase 5: load harness and measured tuning — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prove, on a declared reference node, that one PostgreSQL-authority node serves the 10,000-family model at 2x for 30 minutes inside the spec's SLOs. Then state the measured ceiling and tune only what profiles show matters.

**Architecture:**
- **Seeder.** A SQL-path seeder loads 10,000 families into the current schema in minutes:
  - authority rows come from the product's own commands, run in-process;
  - care history goes through `care.Encode` and `COPY`.
- **Generator.** An open-loop device-mix generator (`baby-tracker-load`) runs in an uncapped container next to the capped app and PostgreSQL containers. It covers refresh, pulls at various cursors, writes, snapshots, idle live sockets, MCP, push-token and Live Activity calls. It records coordinated-omission-safe latencies.
- **Runner.** `Server/scripts/load-node.sh` owns each run: the idle check, the cap verification, the samplers, the profiles and the post-run audit.
- **Tuning.** Every tuning change is its own commit, with before/after numbers from the same node.

**Tech Stack:** Go 1.26 (`pgx/v5`, `coder/websocket`, `net/http/pprof` in `identitytest` builds only), PostgreSQL 17.6 (`pg_stat_statements`), Docker Compose on OrbStack, bash, SwiftPM E2E (`swift test`, no simulator).

**Spec:** `docs/superpowers/specs/2026-10-04-postgres-authority-performance-design.md`, chiefly "Verification → Load" and "Verification → Correctness". Earlier phases: plans `2026-10-04-…-phase1/2`, `2026-10-06-…-phase3`, `2026-10-09-…-phase4`; ledgers in `.superpowers/sdd/2026-10-16-postgres-authority-phase5/` (`phase3-ledger.md`, `phase4-ledger.md`, `phase5-carries-raw.txt`, `progress.md`).

## Global Constraints

**Load model and pass criteria** (exact values from the spec):
- Model at 10,000 families: about 70 refreshes per second, 500 to 700 pulls per second, under 10 care writes per second, and 4,000 open live connections. The harness drives 2x that.
- A SQL seeder loads 10,000 families with realistic history into the new schema "in minutes".
- The capacity load generator gets a device mix: pull, write, refresh and idle live connection.
- CPU, memory and lock profiles are captured per run.
- **PASS** is 2x load sustained for 30 minutes on a declared reference node, with:
  - sync and refresh p95 at most 300 ms;
  - under 0.1% unexpected errors;
  - zero lost or cross-family changes.
- The report states the measured ceiling. Thresholds are tightened after the first measurement. Micro-optimisations (encoders, compression, encodings) happen only when a profile shows they matter.
- Correctness:
  - property tests: a device that pulls in any order converges to server state; concurrent family and account commands never deadlock;
  - the iOS end-to-end suite (two-caregiver sync, owner reinstall, capacity matrix) runs against the new server.

**Reference node** (owner decision, 2026-10-16):
- **The node is this Mac:** Apple M4, 10 cores, 24 GiB, OrbStack.
- **Caps:** the app container is capped at **4 CPU / 8 GB** and PostgreSQL at **4 CPU / 8 GB**. The load generator runs **outside** those caps.
- **Label:** results are labelled **"Apple M4, Docker CPU caps"**, never cloud-equivalent.
- **Machine state:** load runs only when the machine is otherwise idle. Every run passes the idle check (Task 1) and records machine load.

**Safety and approvals:**
- No merge into `main`, push, deploy or homelab contact without the owner's explicit approval.
- Never SSH anywhere, and never use production credentials.
- APNs runs only through the `identitytest` sink, with throwaway keys made per run.

**Shared machine discipline:**
- **Simulator.** iOS work reuses only the shared simulator `<simulator-udid>`. Before any Swift or Xcode run, check `pgrep -fl xcodebuild`. Never create, clone, reset or erase a simulator. This plan's Swift work is `swift test` on the macOS host and needs no simulator.
- **One heavy run at a time.** A heavy run is any `load-node.sh run|seed`, `sync-performance.sh` or full Swift suite. Heavy runs are serialised by the lock in Task 1.
- **Test databases.** Each agent runs at most one `test-db.sh` at a time.
- **Disk.** Keep at least 10 GB free for ordinary work and at least **30 GB** for any 10,000-family seed or run. Check with `df -g /`.
- **Go cache.** Each agent sets a private cache: `export GOCACHE=/path/to/bt-gocache-phase5-task<N>`.
- **Models.** Subagents use opus or sonnet only (never fable). Fall back to sonnet when opus is rate-limited.

**Git:**
- Inside worktrees, use `/usr/bin/git` with simple commands (RTK's git rewrite trips the worktree guard).
- Every commit ends with the session's `Co-Authored-By` trailer.
- Do not edit `TASKS.md` on this branch. Progress goes to `.superpowers/sdd/2026-10-16-postgres-authority-phase5/progress.md`, which is git-excluded.

**Migrations:**
- Numbering is contiguous: the runner refuses gaps with `migration_gap`.
- `main` holds `000034_pumping.sql` (care kind 10) since `eaddcef0`. This branch's one migration is therefore **`000035`** unless `main` has moved again; the rule is in Task 22.

## Review Focus

1. **A saturated generator, or coordinated omission, passes a slow server.**
   - The failure: when the generator falls behind its schedule, it silently offers less load, or it measures only from actual send times, and the run "passes".
   - Expected: every latency is measured from the intended start; a backlog of arrivals is counted, never delayed; and the run fails as `generator_saturated` or `scheduler_lag`.
   - Tests: Task 3 (`TestDispatcherNeverWaitsForSlowOperations`, `TestDispatcherCountsDropsWhenSaturated`, `TestEvaluateFailsSaturatedOrLaggingGenerator`).
2. **A reused seed or fleet carries rotated or expired credentials.**
   - The failure: the run starts with dead tokens, and either signs devices in again behind the scenes or counts mass 401s as load.
   - Expected: the fleet file is the only credential source and is rewritten after every rotation. A seed older than its refresh lineage age is refused, and a 401 is never retried silently.
   - Tests: Task 4 (`TestFleetRoundTripIsPrivateAndAtomic`, `TestStaleSeedIsRefused`) and Task 10 (`TestExpiredAccessIsRefreshedNeverRetriedBlind`).
3. **Another session's work on the shared Mac contaminates a measurement.**
   - The failure: a Swift build or another `test-db.sh` runs during a measurement.
   - Expected: the idle check refuses to start, and a mid-run foreign CPU burst marks the run `contaminated`, which can never PASS.
   - Tests: Task 1 (`TestIdleCheckRefusesForeignHeavyProcess`, `TestHostSamplesFlagContamination`) and Task 3 (`TestEvaluateFailsContaminatedRun`).
4. **The Docker caps are not what the report declares.**
   - The failure: OrbStack ignores a limit, a stale container lacks it, or the generator is accidentally capped.
   - Expected: the runner checks the containers' `HostConfig` and their cgroup files, and runs a CPU burn bound. It refuses on any mismatch.
   - Tests: Task 1 (`TestCapsVerificationRefusesMismatch`, `TestBurnBoundRejectsUncappedCPU`).
5. **The post-run audit passes vacuously or is mis-keyed.**
   - The failure: the audit finds no acknowledged writes, or it compares rows under the wrong family.
   - Expected: an empty ledger fails the run. An injected lost row, a cross-family row and an equal-version different-wire row are each detected.
   - Tests: Task 11 (`TestAuditDetectsLostCrossFamilyAndDiverged`, `TestEmptyLedgerIsVacuous`).

---

## Declared values used across tasks

### Reference node profile (`Server/deploy/load/compose.yaml`, Task 1)

| | app | db | loadgen |
|---|---|---|---|
| CPU | `cpus: 4` | `cpus: 4` | uncapped |
| Memory | `mem_limit: 8g`, `memswap_limit: 8g` | `mem_limit: 8g`, `memswap_limit: 8g`, `shm_size: 1g` | uncapped |
| Image | `identitytest` build of this branch (hosted mode needs the identitytest hosted adapter; see "Why hosted") | `postgres:17.6-alpine@sha256:ef257d85…eee94` | `Server/deploy/load/Dockerfile` |
| Network | `load` 172.31.250.0/24, app .10 | `load`, db .20 | `load`, .100 |

PostgreSQL starting settings (tuned only by Task 16):
- `shared_preload_libraries=pg_stat_statements`, `pg_stat_statements.track=top`;
- `shared_buffers=2GB`, `effective_cache_size=6GB`, `work_mem=16MB`, `maintenance_work_mem=512MB`;
- `max_connections=200`, `max_wal_size=8GB`, `checkpoint_timeout=15min`;
- `random_page_cost=1.1`, `effective_io_concurrency=200`;
- `track_io_timing=on`, `log_lock_waits=on`, `deadlock_timeout=1s`, `log_autovacuum_min_duration=1s`, `log_min_duration_statement=500ms`.

App starting settings:
- **Mode and adapters:** `BT_MODE=hosted`, `BT_IDENTITYTEST_HOSTED_ADAPTER=1`, `BT_SYNC_DIAGNOSTICS=1`, `BT_IDENTITYTEST_PPROF_ADDRESS=:6060` (Task 2).
- **Trusted proxy:** `BT_TRUSTED_PROXY_CIDRS=172.31.250.100/32`. Only the generator may set `X-Forwarded-For`, and it gives each simulated device its own address in 100.64.0.0/10.
- **Pools and admission:** `BT_DB_RUNTIME_MAX_CONNS=32`, `BT_DB_MAINTENANCE_MAX_CONNS=4`, `BT_DB_MCP_MAX_CONNS=2`, `BT_DB_SERVER_MAX_CONNECTIONS=200`, `BT_DB_RESERVED_CONNECTIONS=10`, `BT_ADMISSION_TOTAL=64`, `BT_ADMISSION_MCP_MAX=2`.
- **Live sockets:** `BT_SOCKET_PROCESS_LIMIT=10000`.
- **Push:** `BT_APNS_WAKES_ENABLED=true` and `BT_LIVE_ACTIVITY_PUSH_ENABLED=true`, with `BT_IDENTITYTEST_APNS_SINK=1`, a throwaway P-256 key, and invented team and key IDs, as `Server/scripts/lib/disposable-sync.sh:236-290` does.
- **MCP:** `BT_MCP_ENABLED=true`, with a cursor key and the runtime URL as `Server/scripts/mcp-e2e.sh:50-60` does.
- **Export:** enabled, with keys generated as `Server/scripts/provision-unraid-secrets.sh:95-130` writes them.
- **Public origin:** `BT_PUBLIC_ORIGIN=https://sync.load.test`. The generator sends `Host: sync.load.test`, because MCP requires it.

**Why hosted.** The hosted service is the spec's target, and hosted adds the family entitlement check on every request. Hosted composition needs the identitytest hosted adapter (`cmd/baby-tracker-server/main.go:769`), so the profiled binary is the `identitytest` build. Task 25 records the evidence that the request path is identical: `go list -tags identitytest` against the release build, for every request-path package, differs only in `*_identitytest.go` files.

### Load model at 1x (`loadtest.Model10k`, Task 3)

| Class | 1x | 2x | Notes |
|---|---:|---:|---|
| Refresh (`/v1/auth/refresh-recoverable` + `/v1/auth/refresh-ack`) | 70/s | 140/s | Per-device schedule: interval = devices / rate (40,000 devices: every ~571 s at 1x, ~286 s at 2x), jittered ±30 s |
| Pulls (`POST /v1/sync`, no mutations), open-loop | 600/s | 1,200/s | Plus hint-driven pulls by live devices (reported, not gated separately) |
| Writes (`POST /v1/sync` with 1–3 mutations) | 10/s | 20/s | Upper bound of "under 10/s" |
| Live sockets held | 4,000 | 8,000 | Idle except pings; reconnect on server close or token rotation |
| Snapshot starts (paged to the end) | 0.2/s | 0.4/s | New installs and recovery |
| MCP tool calls | 1/s | 2/s | Over 200 seeded integrations (2% of families) |
| Push-token `PUT` | 1/s | 2/s | |
| Live Activity register/update `PUT` | 0.5/s | 1/s | |
| Sign-in | 0 | 0 | About 0.05/s at 10k families. It is covered functionally by protocol-e2e; stated in the report |

Pull cursor mix:
- 80% at the device's own cursor (normally an empty page);
- 15% rewound 1–50 changes;
- 4% rewound 51–1,000 changes;
- 1% rewound to 0, then paged to the end.

Every page is a request in the `sync_pull` route class.

Write kind mix:

| Kind | Share |
|---|---:|
| feed | 33% |
| diaper | 30% |
| sleep | 20% |
| timer start/stop | 7% |
| supplement | 3% |
| measurement | 2% |
| immunization (kind 9) | 2% |
| pump (kind 10, family-owned, no baby) | 2% |
| growth_profile | 1% |

Operations: create 85%, update 12%, delete 3%. Updates and deletes target only entities the same device created in this run, so versions never race. 15% of write requests carry 2–3 mutations.

### Seed profile R10k (`loadseed.R10k`, Task 4)

**Families and devices.** 10,000 families.
- Accounts: owner and caregiver in every family, plus a viewer in 25%.
- Devices: 4 per family, 40,000 in all — owner 2, caregiver 2, or caregiver 1 plus viewer 1.
- Babies: 85% of families have one, 15% two.

**Tracked history per baby.** The tracked length is drawn from {7, 30, 90, 180, 365} days with weights {25, 30, 25, 15, 5}%, a mean of about 79 days.
- Daily rates: feeds 5, diapers 4, sleeps 2.5, supplements 0.3; a measurement weekly; immunizations on the schedule for the baby's age (about 10 in the first year); one growth profile.
- Pumping: 20% of families log about 3 pumps a day.
- Edits: 10% of entities are updated (version 2–4) and 3% are tombstoned.
- Totals: about **1,100 entities per family** and **~11M `care_entities` rows**.
- Running state: 10% of babies have a running timer, and 5% of families a running pump.

**Side tables.**
- Push tokens on 90% of devices.
- Live Activity registrations on every device (40,000), with active cards for families that have a running timer.
- MCP grants for 2% of families.
- Device cursors: 80% at head, 20% lagging 1–20.
- Care receipts for the seeded writes of the last 30 days.

**Session backlog.** Consumed sessions for the 24 hours before the seed's `as_of`, at the rate of the load the next runs will use (`--backlog-load`, default 2 → ~12M rows; 1 → ~5.8M).

**Audit history.** The account and family chains come only from the provisioning commands, about 15 rows per family. The request path reads only chain heads, so this does not change request cost. The report states it. The audit volume at real retention is decision D1's input (Task 13), not a seed.

**Determinism.** `--seed` fixes every shape, timestamp and care payload. IDs that the product's commands create (accounts, devices, families, tokens) are random by the product's design.

### SLO evaluation (`loadtest.Evaluate`, Task 3)

| Gate | Value (spec) |
|---|---|
| `sync` p95: every `POST /v1/sync` request (pull pages, hint pulls, writes) | ≤ 300 ms |
| `refresh_recoverable` p95 and `refresh_ack` p95 | each ≤ 300 ms |
| Unexpected outcomes / all requests (all routes, live handshakes included) | < 0.1% |
| Lost, cross-family or diverged changes (Task 11 audit) | 0, and a non-empty ledger |
| Unexpected live socket losses (outside a declared storm or restart window) | 0 |
| Generator: dropped arrivals | 0 |
| Generator: scheduler lag p99 | ≤ 20 ms |
| Host contamination | false |

p50, p95, p99 and max are reported for every route class. Task 25 tightens the gates after the first PASS.

**Unexpected outcomes:**
- every 5xx, including 503 `server_busy` and `not_ready`;
- every 429;
- every 4xx other than the generator's own deliberate negative probes, which exist only in Task 19's flood cells and are counted separately;
- every non-`applied` mutation outcome;
- transport errors and client timeouts (10 s);
- protocol decode failures;
- any entity or hint naming another family.

---

## Execution: waves, worktrees and run discipline

**Step 0 (controller, before Wave A).** Merge current `main` into the branch, so every task sees care kind 10 (pumping) and `000034`. This is a local merge into the feature branch, not a merge into `main`; record it in `progress.md`.

```bash
cd <repo>/.worktrees/phase5
/usr/bin/git merge --no-ff main -m "merge main ($(/usr/bin/git rev-parse --short main)): pumping and the logging fix"
/usr/bin/git ls-tree --name-only HEAD Server/migrations/
```

Expected: `000032_baseline.sql`, `000033_immunizations.sql`, `000034_pumping.sql`, `embed.go`. Then run `make server-build` and `make server-test` once, as the baseline. Record the outcome and the duration.

**Waves.** Each task runs in its own isolated worktree off the current branch head (Agent `isolation: "worktree"`, opus implementer, opus or sonnet reviewer). The controller integrates accepted commits with `cherry-pick` or a fast-forward, one at a time. Any two tasks that both edit `cmd/baby-tracker-server/main.go` integrate one after the other, with a rebuild between them.

| Wave | Tasks | Parallel? | Machine use |
|---|---|---|---|
| A | 1, 2, 3, 4, 5, 6, 7, 8, 9 | yes (9 worktrees) | unit tests and one `test-db.sh` per agent; no load runs |
| B | 10, 11 | yes (after 3 and 4 are integrated) | unit tests, one `test-db.sh` each |
| C | 12, 13 | **serial**: the controller runs them on the node | exclusive, heavy |
| D | 14–22 | code in parallel worktrees; **measurements serial** on the node, queued by the controller | exclusive during each measurement |
| E | 23, 24, 25, 26, 27, 28 | **serial** (27 waits for decision D1) | exclusive |

**Run discipline (every heavy run):**
1. `Server/scripts/load-node.sh preflight` passes: idle check, lock and disk (Task 1).
2. Exactly one heavy run holds `/path/to/bt-load.lock`.
3. Every result directory, `/path/to/bt-load/<run-id>/`, holds:
   - `idle.json`, `host-samples.jsonl`, `caps.json`, `docker-stats.jsonl`, `pg-locks.jsonl`, `pg-wait-events.jsonl`;
   - `pg_stat_statements.json`, `app.log` (diagnostics samples), `profiles/`;
   - `result.json`, `audit.json`, `verdict.json`.
4. Nothing in a result directory is a credential, except `private/` (mode 0700): `fleet.jsonl`, `ledger.jsonl` and the secrets. `private/` is never copied into the repo.
5. Curated evidence goes to `docs/verification/<date>-phase5-load-evidence.json` (Task 28): numbers, identities and hashes, never tokens.

**Tuning commits** (Wave D, Tasks 14–22) use this body:

```
perf(<area>): <one-line change>

Before: run <id> (<scenario>, <load>x, <minutes> min): sync p95 <a> ms, refresh p95 <b> ms,
        unexpected <c>%, app CPU <d>%, db CPU <e>%, <the metric this change targets>
After:  run <id> (same scenario): sync p95 …, refresh p95 …, …
Evidence: <profile file and top frames, or pg_stat_statements query id>
```

A change with no measured improvement on its target metric is reverted, not committed. Each run is recorded in the report's tuning log.

## File structure

New:
- `Server/deploy/load/compose.yaml`: the reference-node overlay (caps, PostgreSQL flags, app environment, the `load` network, the `loadgen` service).
- `Server/deploy/load/Dockerfile`: builds `baby-tracker-load` (`-tags identitytest`) into a distroless image.
- `Server/scripts/load-node.sh`: the runner, with subcommands `preflight | up | seed | start | run | profile | audit | down`.
- `Server/scripts/lib/load_host.sh`: the idle check, the lock, the disk check, the host sampler and contamination evaluation.
- `Server/scripts/lib/load_samplers.sh`: the Docker stats, `pg_locks`, wait-event and `pg_stat_statements` samplers.
- `Server/scripts/load_node_test.go`: runner tests with a fake `docker`, `sysctl` and `ps` on `PATH`.
- `Server/internal/loadtest/`:
  - pure: `hist.go`, `arrival.go`, `dispatcher.go`, `recorder.go`, `model.go`, `slo.go`, `ledger.go`, `fleet.go`, `thresholds.json`;
  - generator: `client.go`, `device.go`, `ops_*.go`, `run.go`, `scenario.go`, `audit.go`;
  - each with tests.
- `Server/internal/loadseed/` (`//go:build identitytest`): `spec.go`, `ids.go`, `authority.go`, `care.go`, `side.go`, `backlog.go`, `verify.go`, `seed.go`, and tests.
- `Server/cmd/baby-tracker-load/`: `main.go` (identitytest, subcommands `seed`, `run`, `audit`) and `main_disabled.go` (release stub that exits 2).
- `Server/cmd/baby-tracker-server/`: `pprof.go` (release no-op), `pprof_identitytest.go`, `runtime_limits.go`, and tests.
- `Server/internal/family/command_property_test.go`: the deadlock property over real commands.
- `Server/migrations/000035_family_delete_cascade.sql`: the number is decided by the rule in Task 22.
- `docs/verification/<date>-phase5-load.md` and `docs/verification/<date>-phase5-load-evidence.json`.

Modified (main sites; each task lists its exact files):
- **Observability:** `internal/diagnostics/capacity.go`, `cmd/baby-tracker-server/resource_sampler.go` and `main.go`.
- **Live:** `internal/realtime/hub.go` (stats accessor, chunked sweep).
- **Wake:** `internal/wake/{tracker.go,worker.go,push.go}`.
- **Retention and limiter:** `internal/maintenance/batch.go`, `internal/limits/{memory.go,limiter.go}`.
- **Configuration:** `internal/config/{sync.go,…}`, `internal/protocol/sync.go` (socket limit bounds and default).
- **Request path:** `internal/httpapi/{fault logging, body_gate.go}`, `internal/database/pool.go`.
- **iOS harness:** `Server/scripts/sync-performance.sh`, `Server/scripts/test-performance-runner.sh`, `Server/cmd/baby-tracker-server/apns_sink_identitytest.go`, and `Packages/BabyTracker/Tests/FamilySyncClientE2ETests/BackgroundWakePushE2ETests.swift`.

The existing 32-device capacity harness (`internal/capacity`, `cmd/sync-capacity-client`, `scripts/lib/capacity_control.py`) is left as it is. Its cells, receipts and validated result schema describe closed 32-device Swift/HTTP cells on a restored fixture. The device mix is a sibling (`internal/loadtest`), built for open-loop arrival at 40,000 devices. Extending `workload.go` would change that schema's meaning.

---

## Carries triage

The sources are `phase5-carries-raw.txt` (its line numbers refer to the Phase 3 ledger for 7–268 and to the combined Phase 4 grep for 373–476) and each earlier plan's "Carries out of Phase N" list.

**Perf** marks a carry that can change a load number. Perf rows are measured, and changed only when a profile or threshold shows they matter. Hygiene rows are done because the load runs need clean diagnostics, or because they are cheap and isolated.

| # | Source | Carry | Perf | Disposition |
|---|---|---|---|---|
| C1 | P3 ledger:7, P3 plan:120/252 | D1 audit retention (≈6M rows/day, ~3.1 GB/day at 1x) | yes | **Owner decision D1.** Task 13 measures the inputs; Task 27 implements after the decision |
| C2 | P3:44 | Limiter eviction scans (sample/batch eviction, skip redundant sweep) | yes | Task 19, measured in the IP flood; fixed if eviction appears in the CPU profile at ≥ 2% |
| C3 | P3:44, P3 final R2 | Memory budget: GOMEMLIMIT (~400 MiB on a 512 MiB container), limiter ~180–192 MiB at caps | yes | Task 14 |
| C4 | P3:44 | All-IP-scope flood scenario in the harness | yes | Task 19 |
| C5 | P3:44 | Limiter `Stats()` not read in production | obs | Task 2 (resource sample) |
| C6 | P3:44 | Limiter logging CAS untested | no | Task 9 |
| C7 | P3:50/52 M4 | Verifier probes every monthly partition per page | yes (background) | Task 17, measured; fixed if a verifier pass exceeds 50% of its interval or its probe is in the `pg_stat_statements` top 10 |
| C8 | P3:64 | Background pool contention (verifier, key inventory, sweeps on one connection) | yes | Task 17. The readiness pool split (Phase 3) already removed the fast-probe contention; measure SessionSweep's wait on the one background connection at 2x |
| C9 | P3:64 | Request and maintenance pools lack a connect timeout | no | Task 9 |
| C10 | P3:74 | Swift relaunch flake | — | **Dropped:** moved to and fixed in Phase 3 Task 16 |
| C11 | P3:75/268 | `internal/sync` `TestMutationWaitsOnTheFamilyFenceFirst` intermittent (1 in ~4) | no | Task 6 |
| C12 | P3:91/92 m6 | Unknown token: body read and decoded before 401 | yes (abuse) | Task 19. Phase 3 fix wave A bounds it (60 failed bearers per IP per 10 min, per-IP read cap 8, total 64, 5 s deadline). Measure the CPU per refused 1 MiB body; change only if legitimate p95 moves > 10% |
| C13 | P3:105/106 | Export `expires_at`/`accepted_at` on mixed clocks | no | Task 9 |
| C14 | P3:106 | Import resolve: 29 round trips (charged-bytes updates, duplicated mappings read) | yes (rare path) | Task 20, measured in the import cell; batched only if resolve p95 > 1 s at 2x or sync p95 moves > 10% |
| C15 | P3:123 m5 | High-UUID families starved by the wake scan under sustained overload | yes (overload) | Task 18, checked at Task 24's first failing ramp step |
| C16 | P3:180 M3, P3:204 R4 | Unactionable push-due rows rescanned; recheck fires for tokenless devices | yes | Task 18 |
| C17 | P3:180 M6 | APNs sends while holding the wake worker lock (a slow 200 delays hub wakes up to 15 s a tick) | yes | Task 18 (sink latency 100 ms) |
| C18 | P3:192 | `AdaptiveBatch` never grows back after a halving | yes | Task 17 |
| C19 | P3:196 | Maintenance `-race` tests went from ~12 s to ~70 s | — | **Dropped:** suite runtime, not product behaviour; `server-test` stays inside its budget |
| C20 | P3:200 | More than 16 bad families converge in ceil(n/16) passes; the boundary is untested | no | Task 9 |
| C21 | P3:204/205 R3 | APNs dedupe per tick only | yes | Task 18: fixed if the sink records a duplicate push to one token within 20 minutes |
| C22 | P3:97/219/268 | `internal/mcpapi` `TestHistoryFittingUsesTheCompleteEnvelope` EOF under `-race` load | no | Task 6 |
| C23 | P3:226/227 | Deterministic tick counter for the background-push E2E | no | Task 6 |
| C24 | P3:249/256 R2-M1 | One maintenance slot lets export or purge delay wake ticks (hosted ≥3 or a reserved wake slot) | yes | Task 18 (measure with a purge and an export during 2x); the pool value goes in Task 16 |
| C25 | P3 R2-M2, P3:249 | The tick deadline cancels in-flight APNs (Ambiguous); send budget independent of transaction time | yes | Task 18 |
| C26 | P3 R2-M3 | Tracker 66.2 MiB at the 200k bound (not ≤ 40 MB) | yes | Task 14 (budget table) |
| C27 | P3:249 | Settle and Ambiguous metrics | obs | Task 2 (counters); used in Task 18 |
| C28 | P3:251 R1-M1, P3 plan:262 | Export download streams inside a request-pool transaction | yes | Task 20 (slow-download cell); fixed if runtime-pool wait or sync p95 moves |
| C29 | P3:251 R1-M2 | A slow-but-successful `CheckMany` closes every socket (5 s sweep deadline) | yes | Task 15 |
| C30 | P3:251, P3:268 | `BT_SOCKET_PROCESS_LIMIT` default 1,000 < 4,000 spec | yes | Task 14 |
| C31 | P3:251/268 | Hub and tracker memory in the GOMEMLIMIT budget | yes | Task 14 |
| C32 | P3:251/268 | Reconnect-storm scenario | yes | Task 15 |
| C33 | P3:251/256 | Entitlement UX for lapsed hosted subscriptions | — | **Phase 6:** an owner decision already scheduled there (product UX, not load) |
| C34 | P3:259 | Persist pending settles | — | **Dropped:** the owner-accepted crash window (Phase 3 ruling). It needs a schema change; Task 23 records any wake loss on restart, and only a loss beyond "pushed again on the family's next write" would reopen it |
| C35 | P3:263 m1 | `applySettles` is an all-or-nothing batch | yes (robustness) | Task 18 (isolate the failing settle, plus a metric) |
| C36 | P3:263 m2 | Go h2 abort race may class a written push NotSent | — | **Dropped:** harmless (the body was never sent); the "headers written" rule is kept |
| C37 | P3:263 m3 | Homelab Tailscale Serve proxy not in `BT_TRUSTED_PROXY_CIDRS` | — | **Phase 6:** homelab deploy configuration and runbook; no homelab contact in Phase 5 |
| C38 | P3:263 m4 | Fixed 5 s body-read deadline | no | Task 9 (`BT_BODY_READ_TIMEOUT`) |
| C39 | P4 ledger:105 | Global APNs pause on 429 | — | **Phase 6:** rollout risk note. It needs real Apple traffic to tune; per-job `Retry-After` (≤ 1 min) is kept |
| C40 | P4:192/193 | Fault lines: sentinel errors only by type; timeouts logged as client gone; `server_busy`/`not_ready`/hosted-auth 503 without a fault line; causes at `AccessDigests`, `live.go`, `account.go` | obs | Task 8 (the load runs depend on these lines) |
| C41 | P4:199/201 | Raw `DELETE FROM families` works only by trigger-name order (`import_mappings_shared_baby_fk` lacks a cascade) | no | Task 22 (migration) |
| C42 | P4:208 | Wake tracker progress not flushed on graceful stop | yes (restart) | Task 23 |
| C43 | P2 plan:1130/1154 | Live CapacityMatrix through `sync-performance.sh` (reads the retired `security_projection_head`) | — | Tasks 7 and 26 |
| C44 | P2 plan:1154 | `check-migration-overlap.sh` | — | **Dropped:** the script no longer exists. The runner's `migration_gap` refusal, the manifest's contiguity check (`internal/migrate/manifest.go:51`) and Task 22's numbering step cover it |
| C45 | P3 plan:263/4016 | Size the request pool from measurement; check the 2 background connections | yes | Task 16 (pool); Task 17 (background) |
| C46 | P3 plan:4017 | Measure the in-memory limiter at 2x | yes | Tasks 14 and 19 |
| C47 | P3 plan:4018 | Retention throughput at 2x (140 consumed sessions/s) against `SessionBatch` and `RetentionInterval` | yes | Task 17 |
| C48 | P3 plan:4019 | Wake worker cost with 40k device rows | yes | Task 18 |
| C49 | P3 plan:256/4020, P2:49 | HOT and fillfactor on `care_entities` | yes | Task 20 |
| C50 | P3 plan:4021 R1 | Family `lock_timeout` or bounded waiters | yes | Task 20 (hot-family cell) |
| C51 | P3 plan:261/4022 | MCP reads and history in one transaction | yes | Task 20 |
| C52 | P3 plan:4024 R2 C3 | Mass-snapshot test | yes | Task 23 |
| C53 | P3 plan:4025 | Recorded Swift and mcp-e2e flakes | no | Task 6 (server); Task 28 gates allow the one documented Swift rerun |
| C54 | P4 plan:4829 | Import duration on the reference node against the 15-minute L10k target | — | **Dropped:** the legacy importer was deleted at `b474c4b0` after the homelab cutover (2026-10-10); no legacy install remains |
| C55 | P4 plan:4830 | Live Activity worker at 10,000 families (minute sweeps, claims with 40,000 registrations, change-feed flush) | yes | Task 18 |
| C56 | P4 plan:4831 | One APNs connection per environment, shared by wakes and Live Activity, under Apple's limits | yes | Task 18 |
| C57 | P4 plan:4832 | `main`'s Live Activity hardening tests under `-race` at load | no | Task 18 (`-race -count=5` beside a 4-CPU burn container) |
| C58 | P4 plan:4833 | Wake backstop and the dirty sweep in the minutes after a cutover | yes | Task 23 (restart after downtime) |
| C59 | spec | Thresholds tightened after the first measurement | — | Task 25 |

---

## Wave A

### Task 1: Reference-node stack, idle check, lock, caps verification and samplers

**Files:**
- Create: `Server/deploy/load/compose.yaml`, `Server/deploy/load/Dockerfile`
- Create: `Server/scripts/load-node.sh`, `Server/scripts/lib/load_host.sh`, `Server/scripts/lib/load_samplers.sh`
- Test: `Server/scripts/load_node_test.go` (package `scripts_test`, the pattern of `protocol_e2e_test.go`: a fake `docker`/`sysctl`/`ps`/`df` on `PATH` that records arguments and prints canned output)

**Interfaces:**
- Produces, for Tasks 12–28: the `load-node.sh` subcommands below.

  ```
  load-node.sh preflight --families N            # idle check, lock, disk; writes $RUN/idle.json
  load-node.sh up        --run RUN               # secrets, compose up db, migrate, authority init, caps verify, pg_stat_statements
  load-node.sh verify-caps --run RUN             # docker inspect + cgroup files + burn bound → $RUN/caps.json; refuses on mismatch
  load-node.sh seed      --run RUN --families N --seed S [--backlog-load L] [--as-of RFC3339]
  load-node.sh start     --run RUN               # app up, readiness, runtime-limits log check
  load-node.sh run       --run RUN --scenario soak|ramp|storm|restart|mass-snapshot|hot-family|flood|import|export \
                         --load L --warmup D --minutes M [--profile-at 5m,15m,25m] [--thresholds FILE]
  load-node.sh profile   --run RUN --label X     # cpu 30 s, heap, mutex, block, goroutine → $RUN/profiles/X-*
  load-node.sh audit     --run RUN
  load-node.sh down      --run RUN [--keep-volume]
  ```
- Produces: the environment variable `BT_LOAD_RUN` (absolute run directory under `/path/to/bt-load/`) and the files listed in "Run discipline".

- [ ] **Step 1: Write the failing runner tests.** In `Server/scripts/load_node_test.go`:

```go
package scripts_test

// Each test builds a fake bin directory whose docker/sysctl/ps/df print the
// canned output named by environment variables and append their arguments
// to $FAKE_LOG, then runs load-node.sh with PATH=fake:/usr/bin:/bin.

func TestIdleCheckRefusesForeignHeavyProcess(t *testing.T) {
	env := fakeHost(t, fakeHostOptions{LoadAvg: "{ 0.80 0.90 1.00 }", PS: " 95.0 swift-frontend\n  3.0 Terminal\n", DFAvailGB: 120})
	out, code := runLoadNode(t, env, "preflight", "--families", "10000")
	if code == 0 || !strings.Contains(out, "machine not idle: swift-frontend") {
		t.Fatalf("exit %d, output %q; want refusal naming swift-frontend", code, out)
	}
}

func TestIdleCheckRefusesHighLoadAverage(t *testing.T) {
	env := fakeHost(t, fakeHostOptions{LoadAvg: "{ 4.50 3.20 2.00 }", PS: "  2.0 Terminal\n", DFAvailGB: 120})
	out, code := runLoadNode(t, env, "preflight", "--families", "10000")
	if code == 0 || !strings.Contains(out, "load average 4.50 >= 3.00") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

func TestPreflightRefusesLowDiskFor10k(t *testing.T) {
	env := fakeHost(t, fakeHostOptions{LoadAvg: "{ 0.5 0.5 0.5 }", PS: "  1.0 Terminal\n", DFAvailGB: 25})
	out, code := runLoadNode(t, env, "preflight", "--families", "10000")
	if code == 0 || !strings.Contains(out, "free disk 25 GB < 30 GB") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

func TestLockIsExclusive(t *testing.T) {
	env := fakeHost(t, idleHost())
	lock := filepath.Join(env.TempDir, "bt-load.lock")
	if err := os.Mkdir(lock, 0o700); err != nil { t.Fatal(err) }
	os.WriteFile(filepath.Join(lock, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600) // a live pid
	out, code := runLoadNode(t, env.With("BT_LOAD_LOCK", lock), "preflight", "--families", "100")
	if code == 0 || !strings.Contains(out, "another load run holds") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

func TestCapsVerificationRefusesMismatch(t *testing.T) {
	// docker inspect prints app NanoCpus 2000000000: refuse.
	env := fakeHost(t, idleHost()).With("FAKE_INSPECT_APP", "2000000000 8589934592 8589934592").
		With("FAKE_INSPECT_DB", "4000000000 8589934592 8589934592")
	out, code := runLoadNode(t, env, "verify-caps", "--run", env.Run)
	if code == 0 || !strings.Contains(out, "app NanoCpus 2000000000 != 4000000000") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

func TestBurnBoundRejectsUncappedCPU(t *testing.T) {
	env := fakeHost(t, idleHost()).With("FAKE_BURN_CPU", "612.40%")
	out, code := runLoadNode(t, env, "verify-caps", "--run", env.Run)
	if code == 0 || !strings.Contains(out, "burn container used 612.40% > 420%") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

func TestHostSamplesFlagContamination(t *testing.T) {
	// 7 samples (70 s) where a foreign process averages 80% CPU: contaminated.
	samples := strings.Repeat(`{"at":"…","load1":2.1,"top":[{"pcpu":80.0,"comm":"swift-frontend"}]}`+"\n", 7)
	out, code := runEvaluateHost(t, samples)
	if code != 0 || !strings.Contains(out, `"contaminated":true`) || !strings.Contains(out, "swift-frontend") {
		t.Fatalf("exit %d, output %q", code, out)
	}
}

func TestComposeOverlayDeclaresTheReferenceNode(t *testing.T) {
	raw, err := os.ReadFile("../deploy/load/compose.yaml")
	if err != nil { t.Fatal(err) }
	for _, want := range []string{"cpus: 4", "mem_limit: 8g", "memswap_limit: 8g", "shared_preload_libraries=pg_stat_statements",
		"BT_TRUSTED_PROXY_CIDRS: 172.31.250.100/32", "BT_IDENTITYTEST_APNS_SINK: \"1\"", "subnet: 172.31.250.0/24"} {
		if !strings.Contains(string(raw), want) { t.Errorf("overlay lacks %q", want) }
	}
	if strings.Contains(sectionOf(string(raw), "loadgen"), "cpus:") { t.Error("the generator must not be capped") }
}
```

Write the helpers `fakeHost`, `idleHost`, `runLoadNode`, `runEvaluateHost` and `sectionOf` in the same file. `fakeHost` writes bash scripts into `t.TempDir()/bin`:
- `docker` answers `inspect -f …` from `FAKE_INSPECT_APP`/`FAKE_INSPECT_DB`/`FAKE_INSPECT_LOADGEN` (default `0 0 0` for loadgen);
- `docker stats --no-stream --format {{.CPUPerc}}` from `FAKE_BURN_CPU`;
- `sysctl -n vm.loadavg` from `LoadAvg`;
- `ps -Ao pcpu=,comm= -r` from `PS`;
- `df -g /` prints a line with `DFAvailGB` in column 4.

- [ ] **Step 2: Run to see them fail.**

```bash
export GOCACHE=/path/to/bt-gocache-phase5-task1
cd Server && go test ./scripts -run 'IdleCheck|Preflight|Lock|Caps|Burn|HostSamples|ComposeOverlay' -count=1
```

Expected: FAIL (`load-node.sh` does not exist).

- [ ] **Step 3: Write `Server/scripts/lib/load_host.sh`.**

```bash
# Host discipline for Phase 5 load runs. Sourced by load-node.sh.
BT_LOAD_LOCK=${BT_LOAD_LOCK:-/path/to/bt-load.lock}
# Processes that mean another session is building, testing or simulating.
BT_LOAD_FOREIGN='xcodebuild|swift-build|swift-frontend|swift-test|swiftc|Simulator|go-build|compile|test-db\.sh|protocol-e2e\.sh|mcp-e2e\.sh|sync-performance\.sh|chromium|Chromium'

bt_load_take_lock() {
  if mkdir "$BT_LOAD_LOCK" 2>/dev/null; then echo $$ > "$BT_LOAD_LOCK/pid"; return 0; fi
  local holder; holder=$(cat "$BT_LOAD_LOCK/pid" 2>/dev/null || true)
  if [[ -n $holder ]] && kill -0 "$holder" 2>/dev/null; then
    echo "another load run holds $BT_LOAD_LOCK (pid $holder)" >&2; return 1
  fi
  echo "removing stale load lock (pid ${holder:-none} is gone)" >&2
  rm -rf "$BT_LOAD_LOCK" && mkdir "$BT_LOAD_LOCK" && echo $$ > "$BT_LOAD_LOCK/pid"
}
bt_load_release_lock() { [[ $(cat "$BT_LOAD_LOCK/pid" 2>/dev/null) == "$$" ]] && rm -rf "$BT_LOAD_LOCK"; return 0; }

# bt_load_idle_once prints a JSON sample and returns non-zero with a reason
# when the machine is not idle: 1-minute load >= 3.00 or 5-minute >= 4.00
# (10 cores), any foreign heavy process, or any other process above 50% CPU
# that is not OrbStack.
bt_load_idle_once() {
  local avg l1 l5 top reason=""
  avg=$(sysctl -n vm.loadavg); read -r _ l1 l5 _ _ <<<"$avg"
  top=$(ps -Ao pcpu=,comm= -r | head -8)
  awk -v a="$l1" 'BEGIN{exit !(a>=3.0)}' && reason="load average $l1 >= 3.00"
  awk -v a="$l5" 'BEGIN{exit !(a>=4.0)}' && reason="5-minute load average $l5 >= 4.00"
  local foreign; foreign=$(grep -E "$BT_LOAD_FOREIGN" <<<"$top" | awk '{print $2}' | head -1)
  [[ -z $foreign ]] || reason="machine not idle: $foreign"
  local hot; hot=$(awk '$1>50 && $2 !~ /OrbStack|com.orbstack|vmgr|baby-tracker-load/ {print $2; exit}' <<<"$top")
  [[ -z $hot ]] || reason="machine not idle: $hot above 50% CPU"
  jq -cn --arg at "$(date -u +%FT%TZ)" --arg avg "$avg" --arg top "$top" --arg reason "$reason" \
    '{at:$at,loadavg:$avg,top:($top|split("\n")),reason:$reason}'
  [[ -z $reason ]] || { echo "$reason" >&2; return 1; }
}

# Three samples 20 s apart must all pass (BT_LOAD_IDLE_SAMPLES/INTERVAL for tests).
bt_load_idle_check() {
  local out=$1 n=${BT_LOAD_IDLE_SAMPLES:-3} gap=${BT_LOAD_IDLE_INTERVAL:-20} i
  : > "$out"
  for ((i=1; i<=n; i++)); do bt_load_idle_once >> "$out" || return 1; ((i<n)) && sleep "$gap"; done
  local others; others=$(docker ps --format '{{.Names}}' | grep -v "^${BT_LOAD_PROJECT:-bt-load}-" || true)
  [[ -z $others ]] || echo "note: other containers running: $(tr '\n' ' ' <<<"$others")" | tee -a "$out" >&2
}

bt_load_disk_check() {
  local families=$1 need=10 avail
  ((families >= 1000)) && need=30
  avail=$(df -g / | awk 'NR==2{print $4}')
  ((avail >= need)) || { echo "free disk $avail GB < $need GB" >&2; return 1; }
}

# Every 10 s: load average and the top 8 processes, until the file named by
# $2 disappears.
bt_load_host_sampler() {
  local out=$1 alive=$2
  while [[ -e $alive ]]; do
    jq -cn --arg at "$(date -u +%FT%TZ)" --arg avg "$(sysctl -n vm.loadavg)" \
      --arg top "$(ps -Ao pcpu=,comm= -r | head -8)" \
      '{at:$at,loadavg:$avg,top:($top|split("\n")|map(select(length>0)|capture("^ *(?<pcpu>[0-9.]+) +(?<comm>.*)$")|.pcpu|=tonumber))}' >> "$out"
    sleep 10
  done
}
```

Contamination evaluation (`bt_load_evaluate_host <samples>`) is a `jq` program. Ignoring OrbStack, the VM and `baby-tracker-load`, it groups the samples by `comm` and flags a run `contaminated` when any other process's mean CPU over ≥ 6 consecutive samples (60 s) is > 50%, or any `BT_LOAD_FOREIGN` process appears in ≥ 2 samples. It prints `{"contaminated":bool,"offenders":[…],"max_load1":x}`.

- [ ] **Step 4: Write `Server/deploy/load/compose.yaml`.** Use the values in "Reference node profile". The skeleton:

```yaml
# Phase 5 reference node, labelled "Apple M4, Docker CPU caps": app and db
# each 4 CPU / 8 GiB with memory+swap equal (no swap). The loadgen service is
# uncapped by design (owner decision 2026-10-16). Used only by load-node.sh:
#   docker compose -p $BT_LOAD_PROJECT -f Server/compose.yaml -f Server/deploy/load/compose.yaml …
services:
  db:
    cpus: 4
    mem_limit: 8g
    memswap_limit: 8g
    shm_size: 1g
    command: [postgres, -c, shared_preload_libraries=pg_stat_statements, -c, pg_stat_statements.track=top,
              -c, shared_buffers=2GB, -c, effective_cache_size=6GB, -c, work_mem=16MB, -c, maintenance_work_mem=512MB,
              -c, max_connections=200, -c, max_wal_size=8GB, -c, checkpoint_timeout=15min, -c, random_page_cost=1.1,
              -c, effective_io_concurrency=200, -c, track_io_timing=on, -c, log_lock_waits=on, -c, deadlock_timeout=1s,
              -c, log_autovacuum_min_duration=1s, -c, log_min_duration_statement=500ms]
    ports: ["127.0.0.1:${BT_LOAD_DB_PORT:-0}:5432"]
    networks: {default: {}, load: {ipv4_address: 172.31.250.20}}
  app:
    cpus: 4
    mem_limit: 8g
    memswap_limit: 8g
    ulimits: {nofile: {soft: 65536, hard: 65536}}
    build: {context: ., args: {BT_BUILD_TAGS: identitytest, BT_SOURCE_SHA: "${BT_SOURCE_SHA:?}"}}
    environment:
      BT_MODE: hosted
      BT_IDENTITYTEST_HOSTED_ADAPTER: "1"
      BT_SYNC_DIAGNOSTICS: "1"
      BT_IDENTITYTEST_PPROF_ADDRESS: ":6060"
      BT_TRUSTED_PROXY_CIDRS: 172.31.250.100/32
      BT_SOCKET_PROCESS_LIMIT: "${BT_LOAD_SOCKET_LIMIT:-10000}"
      BT_DB_RUNTIME_MAX_CONNS: "${BT_LOAD_RUNTIME_CONNS:-32}"
      BT_DB_MAINTENANCE_MAX_CONNS: "${BT_LOAD_MAINTENANCE_CONNS:-4}"
      BT_DB_MCP_MAX_CONNS: "2"
      BT_DB_SERVER_MAX_CONNECTIONS: "200"
      BT_DB_RESERVED_CONNECTIONS: "10"
      BT_ADMISSION_TOTAL: "${BT_LOAD_ADMISSION:-64}"
      BT_ADMISSION_MCP_MAX: "2"
      BT_APNS_WAKES_ENABLED: "true"
      BT_LIVE_ACTIVITY_PUSH_ENABLED: "true"
      BT_IDENTITYTEST_APNS_SINK: "1"
      BT_IDENTITYTEST_APNS_SINK_LATENCY_MS: "${BT_LOAD_APNS_LATENCY_MS:-100}"   # Task 18 adds the variable
      BT_APNS_TEAM_ID: LOADTEAM01
      BT_APNS_KEY_ID: LOADKEY001
      BT_APNS_BUNDLE_ID: com.example.babytracker.load
      BT_APNS_KEY_FILE: /run/secrets/apns_key
      BT_MCP_ENABLED: "true"
      BT_MCP_DATABASE_URL_FILE: /run/secrets/runtime_database_url
      BT_MCP_CURSOR_KEY_FILE: /run/mcp/cursor_key
      BT_EXPORT_DATABASE_URL_FILE: /run/secrets/maintenance_database_url
      BT_EXPORT_KEYS_FILE: /run/keyrings/export-keys.json
      BT_EXPORT_KEY_ID: export.v1
    secrets: [apns_key]
    ports: ["127.0.0.1:${BT_LOAD_PPROF_PORT:-0}:6060"]
    networks: {default: {}, load: {ipv4_address: 172.31.250.10}}
  migrate:
    build: {context: ., args: {BT_BUILD_TAGS: identitytest, BT_SOURCE_SHA: "${BT_SOURCE_SHA:?}"}}
  loadgen:
    build: {context: ., dockerfile: deploy/load/Dockerfile}
    profiles: [tools]
    ulimits: {nofile: {soft: 262144, hard: 262144}}
    volumes: ["${BT_LOAD_RUN:?}:/run/load"]
    networks: {load: {ipv4_address: 172.31.250.100}}
networks:
  load:
    ipam: {config: [{subnet: 172.31.250.0/24}]}
secrets:
  apns_key: {file: "${BT_SECRETS_DIR:?}/apns_key.p8"}
```

`Server/deploy/load/Dockerfile` uses the server Dockerfile's pinned `golang:1.26.2-alpine` build stage. It runs `go build -tags identitytest -trimpath -o /out/baby-tracker-load ./cmd/baby-tracker-load` and copies the binary into the same pinned distroless image, as `USER 65532`.

- [ ] **Step 5: Write `load-node.sh`.**

**Option discipline.** Port the same option discipline as `sync-performance.sh`:
- refuse any inherited `BT_*` variables except `BT_LOAD_*`;
- runs live only under `/path/to/bt-load/`;
- symlinked output is refused.

**`up`.** Generate the secrets as `disposable-sync.sh:84-140` does. Add:
- `apns_key.p8` (a throwaway P-256 key, 0600);
- `mcp/cursor_key`;
- `keyrings/export-keys.json`.

Then:
- export `BT_SERVER_INSTANCE_ID=70000000-0000-4000-8000-0000000a0d01`, `BT_RECOVERY_EPOCH=1`, `BT_APPLE_AUDIENCE=com.example.babytracker.load`, `BT_PUBLIC_ORIGIN=https://sync.load.test`, `BT_SOURCE_SHA=$(/usr/bin/git rev-parse HEAD)`;
- run `compose up -d --wait db`, `compose build app migrate loadgen`, `compose run --rm --no-deps migrate`, and `compose run --rm --no-deps migrate authority init --instance-id … --epoch 1 --confirm-instance …`;
- then `verify-caps`;
- then `docker exec $db psql -U postgres -d postgres -c 'CREATE EXTENSION IF NOT EXISTS pg_stat_statements'`. The extension lives in the `postgres` maintenance database, so the `babytracker` catalog and the readiness privilege audit are untouched.

**`verify-caps`** writes `caps.json` and refuses on any mismatch:
1. `docker inspect -f '{{.HostConfig.NanoCpus}} {{.HostConfig.Memory}} {{.HostConfig.MemorySwap}}'` reads `4000000000 8589934592 8589934592` for app and db, and `0 0 0` for loadgen (inspect a `compose create loadgen` container).
2. `docker exec $db cat /sys/fs/cgroup/cpu.max /sys/fs/cgroup/memory.max` reads `400000 100000` and `8589934592`.
3. The app check (after `start`): the startup log line `runtime limits` (Task 2) shows `gomaxprocs=4` and `cgroup_memory_max=8589934592`.
4. **Burn bound:**
   - run `docker run --rm -d --name ${BT_LOAD_PROJECT}-burn --cpus 4 --memory 8g postgres:17.6-alpine@sha256:ef257d85… sh -c 'for i in 1 2 3 4 5 6 7 8; do (while :; do :; done) & done; sleep 25'`;
   - sample `docker stats --no-stream --format '{{.CPUPerc}}'` three times between 10 and 20 s;
   - each sample must be ≤ 420% and the mean ≥ 350%; then remove the container.
5. Record the VM facts: `docker info --format '{{.NCPU}} {{.MemTotal}}'`, `orb config show | grep -E '^(cpu|memory_mib):'`, `sysctl -n hw.memsize hw.ncpu`, and the macOS version.

**`seed`, `run`, `audit`** call `compose run --rm loadgen <subcommand>` (Tasks 4, 10 and 11) with `/run/load` as the run directory.

**`run`:**
1. Starts the samplers: host, Docker stats, PostgreSQL. Each runs until the sentinel file `$RUN/.sampling` is removed.
2. Resets `pg_stat_statements` at the start of the measurement window. The generator writes `$RUN/window-start` when warm-up ends; the runner waits for it.
3. Captures profiles at each `--profile-at` offset.
4. Dumps `pg_stat_statements` at the end: the top 50 rows by `total_exec_time` with `calls`, `mean_exec_time`, `rows`, `shared_blks_hit`, `shared_blks_read`, `temp_blks_written` and the `query` text.
5. Copies `docker logs app` to `app.log`.
6. Evaluates the host samples into `host.json`.

**`profile`** fetches from the published pprof port, writing each to `$RUN/profiles/<label>-<kind>.pb.gz`:
- `/debug/pprof/profile?seconds=30`;
- `/debug/pprof/heap`;
- `/debug/pprof/mutex`;
- `/debug/pprof/block`;
- `/debug/pprof/goroutine?debug=1`.

**`down`** runs `compose down --remove-orphans`, plus `--volumes` unless `--keep-volume`. It always releases the lock.

`Server/scripts/lib/load_samplers.sh` holds the sampler loops:
- **Docker stats** every 5 s: `docker stats --no-stream --format '{{json .}}' app db loadgen`.
- **PostgreSQL** every 1 s, through `docker exec $db psql -XAt -U postgres -d babytracker`:
  - one JSON row of blocked locks: `SELECT json_agg(…) FROM pg_locks l JOIN pg_stat_activity a USING (pid) WHERE NOT l.granted`;
  - wait-event counts: `SELECT wait_event_type, wait_event, count(*) FROM pg_stat_activity WHERE state <> 'idle' GROUP BY 1,2`.

- [ ] **Step 6: Run the tests green.** Same command as Step 2. Expected: PASS. Then `bash -n Server/scripts/load-node.sh Server/scripts/lib/load_*.sh`.

- [ ] **Step 7: A real `up`/`verify-caps`/`down` on this Mac** — the only Docker use in this task. Skip it and record "not run" if the idle check refuses.

```bash
BT_LOAD_RUN=/path/to/bt-load/task1-$(date +%s) bash Server/scripts/load-node.sh preflight --families 100
bash Server/scripts/load-node.sh up --run "$BT_LOAD_RUN" && bash Server/scripts/load-node.sh down --run "$BT_LOAD_RUN"
cat "$BT_LOAD_RUN/caps.json"
```

Expected: `caps.json` shows app and db at 4 CPU / 8 GiB, loadgen uncapped, and the burn mean between 350% and 420%. Record the OrbStack `memory_mib` (12288 on 2026-10-16) for decision D2.

- [ ] **Step 8: Commit.**

```bash
/usr/bin/git add Server/deploy/load Server/scripts/load-node.sh Server/scripts/lib/load_host.sh Server/scripts/lib/load_samplers.sh Server/scripts/load_node_test.go
/usr/bin/git commit -m "feat(load): the Phase 5 reference node: capped app and db, uncapped generator, idle check, lock, caps proof and samplers"
```

---

### Task 2: Server profiling and runtime observability

**Files:**
- Create: `Server/cmd/baby-tracker-server/pprof.go` (`//go:build !identitytest`), `pprof_identitytest.go`, `pprof_identitytest_test.go`
- Create: `Server/cmd/baby-tracker-server/runtime_limits.go`, `runtime_limits_test.go`
- Modify: `Server/internal/diagnostics/capacity.go` (fields on `ResourceSample`), `Server/cmd/baby-tracker-server/resource_sampler.go`, `main.go`
- Modify: `Server/internal/realtime/hub.go` (`Stats()`), `Server/internal/wake/tracker.go` (`Len()`), `Server/internal/wake/worker.go` (cumulative `Totals()`)
- Test: `Server/internal/realtime/hub_test.go`, `Server/internal/wake/worker_test.go`, `Server/cmd/baby-tracker-server/resource_sampler_test.go`

**Interfaces:**
- Produces: `startProfiling(logger *slog.Logger) (stop func())`.
  - `identitytest` build: when `BT_IDENTITYTEST_PPROF_ADDRESS` is set, it serves `net/http/pprof` on its own listener (never on the API handler) and sets `runtime.SetMutexProfileFraction(100)` and `runtime.SetBlockProfileRate(10_000_000)`.
  - Release build: a no-op that returns `func(){}`.
- Produces: `func cgroupLimits(read func(string) ([]byte, error)) (cpuQuota float64, memoryMax int64, ok bool)`, and the startup log line `"runtime limits"` with `gomaxprocs`, `gomemlimit`, `cgroup_cpu_quota` and `cgroup_memory_max`.
- Produces: `func (hub *Hub) Stats() realtime.HubStats` (`Leases, Reservations int`); `func (t *Tracker) Len() int`; `func (w *Worker) Totals() wake.Totals` (`Ticks, Flushed, Woken, Pushed, PushFailed, NotSent, Ambiguous, SettlesPending, SettlesFailed uint64`).
- Produces these `diagnostics.ResourceSample` fields (all `omitempty`, fixed labels only):

  ```go
  Runtime *RuntimeSample `json:"runtime,omitempty"` // HeapInuse, HeapObjects, NumGC, PauseTotalNS, GOMEMLIMIT, GOMAXPROCS
  Limiter *LimiterSample `json:"limiter,omitempty"` // VerifiedFull uint64; IPEvictions map[string]uint64
  Live    *LiveSample    `json:"live,omitempty"`    // Leases, Reservations int
  Wake    *WakeSample    `json:"wake,omitempty"`    // TrackerEntries int; TrackerDropped uint64; wake.Totals fields
  ```

- [ ] **Step 1: Failing tests.**

```go
//go:build identitytest

func TestPprofListensOnlyOnItsOwnAddress(t *testing.T) {
	t.Setenv("BT_IDENTITYTEST_PPROF_ADDRESS", "127.0.0.1:0")
	stop := startProfiling(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer stop()
	address := profilingAddress() // test hook: the bound listener's address
	response, err := http.Get("http://" + address + "/debug/pprof/heap?debug=1")
	if err != nil || response.StatusCode != 200 { t.Fatalf("heap profile: %v %v", err, response) }
	// The API handler never serves pprof.
	api := httptest.NewServer(httpapi.New(minimalOptions(t)))
	defer api.Close()
	if r, _ := http.Get(api.URL + "/debug/pprof/"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("API handler served pprof: %d", r.StatusCode)
	}
}
```

The untagged `pprof_test.go`: `startProfiling` returns a non-nil stop, and no listener exists even with the variable set. Also assert that `go list -deps ./cmd/baby-tracker-server` (release) does not include `net/http/pprof`. Use `exec.Command("go","list","-deps","./cmd/baby-tracker-server")` and check the output.

```go
func TestCgroupLimitsParsesV2Files(t *testing.T) {
	files := map[string]string{"/sys/fs/cgroup/cpu.max": "400000 100000\n", "/sys/fs/cgroup/memory.max": "8589934592\n"}
	quota, memory, ok := cgroupLimits(func(p string) ([]byte, error) { return []byte(files[p]), nil })
	if !ok || quota != 4 || memory != 8589934592 { t.Fatalf("got %v %v %v", quota, memory, ok) }
	files["/sys/fs/cgroup/memory.max"] = "max\n"
	if _, memory, _ := cgroupLimits(func(p string) ([]byte, error) { return []byte(files[p]), nil }); memory != 0 {
		t.Fatalf("unlimited memory read as %d", memory)
	}
}
```

Hub: `TestHubStatsCountsLeasesAndReservations` — reserve 2 and bind 1 through the existing test helpers, then expect `{Leases:1, Reservations:1}`. Worker: `TestWorkerTotalsAccumulateAcrossTicks` — two ticks with the existing fakes, then expect `Ticks == 2` and `Flushed` equal to the sum. Sampler: `TestResourceSampleCarriesLimiterLiveAndWake` — the sampler's `read` closure, given stub sources, fills all four new fields.

- [ ] **Step 2: Run, expect FAIL.**

```bash
export GOCACHE=/path/to/bt-gocache-phase5-task2
cd Server && go test ./cmd/baby-tracker-server ./internal/realtime ./internal/wake ./internal/diagnostics -run 'Pprof|Cgroup|HubStats|WorkerTotals|ResourceSample' -count=1
go test -tags identitytest ./cmd/baby-tracker-server -run Pprof -count=1
```

- [ ] **Step 3: Implement.**

`pprof_identitytest.go`:

```go
//go:build identitytest

package main

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"sync/atomic"
	"time"
)

const identityTestPprofEnv = "BT_IDENTITYTEST_PPROF_ADDRESS"

var profilingListener atomic.Pointer[net.Listener]

// startProfiling serves net/http/pprof on its own listener when
// BT_IDENTITYTEST_PPROF_ADDRESS is set (identitytest builds only; the release
// binary links no pprof). Mutex and block profiling are sampled at rates
// low enough for the 30-minute soak: 1 in 100 contention events and blocking
// of 10 ms or more.
func startProfiling(logger *slog.Logger) func() {
	address := os.Getenv(identityTestPprofEnv)
	if address == "" {
		return func() {}
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		logger.Error("pprof listener", slog.String("error", err.Error()))
		return func() {}
	}
	profilingListener.Store(&listener)
	runtime.SetMutexProfileFraction(100)
	runtime.SetBlockProfileRate(int((10 * time.Millisecond).Nanoseconds()))
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	logger.Info("pprof listening", slog.String("address", listener.Addr().String()))
	return func() { _ = server.Close() }
}

func profilingAddress() string {
	if l := profilingListener.Load(); l != nil {
		return (*l).Addr().String()
	}
	return ""
}
```

`pprof.go` (`//go:build !identitytest`): `func startProfiling(*slog.Logger) func() { return func() {} }`.

`runtime_limits.go`:
- `cgroupLimits` parses `cpu.max` ("max" or "quota period") and `memory.max` ("max" or bytes).
- `logRuntimeLimits(logger)` logs `runtime.GOMAXPROCS(0)` and `debug.SetMemoryLimit(-1)` (reads the limit without changing it), plus both cgroup values.

In `main.go`, right after `slog.SetDefault(logger)`, call `stopProfiling := startProfiling(logger); defer stopProfiling()` and `logRuntimeLimits(logger)`.

Accessors:
- `Hub.Stats()` reads the lengths under `hub.mu`.
- `Tracker.Len()` reads under the tracker's lock.
- `Worker` gains `totals` (a struct of `atomic.Uint64`), incremented where `Tick` already counts `Stats`, and in `push.go` where NotSent, Ambiguous and settle outcomes are classified. `Totals()` copies it.

`readResources` gains parameters for the four sources. `main.go` passes `limiter.(*limits.Service).Stats` (type-assert once at composition), `options.LiveHub.Stats`, `wakeTracker.Len`, `wakeTracker.Dropped` and `wakeWorker.Totals`. Note: `wakeWorker` is built after the sampler starts today; move the sampler start below `newWakeWorker`.

- [ ] **Step 4: Run green.** The Step 2 commands, then:

```bash
cd Server && go vet ./... && go vet -tags identitytest ./...
BT_TEST_PACKAGES='./cmd/baby-tracker-server ./internal/realtime ./internal/wake ./internal/diagnostics' bash scripts/test-db.sh
```

Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
/usr/bin/git add Server/cmd/baby-tracker-server Server/internal/diagnostics Server/internal/realtime/hub.go Server/internal/realtime/hub_test.go Server/internal/wake
/usr/bin/git commit -m "feat(diagnostics): identitytest pprof listener, runtime limits at startup, limiter/live/wake/runtime in resource samples"
```

---

### Task 3: The load-test core: histogram, open-loop arrivals, dispatcher, recorder, model, SLO and ledger

**Files:**
- Create: `Server/internal/loadtest/{hist.go,arrival.go,dispatcher.go,recorder.go,model.go,slo.go,ledger.go,thresholds.json}`
- Test: `Server/internal/loadtest/{hist_test.go,arrival_test.go,dispatcher_test.go,model_test.go,slo_test.go,ledger_test.go}`

**Interfaces** (all untagged, pure Go, no database):
- Produces:

  ```go
  type Histogram struct{ Counts [histBuckets]uint64; Total, MaxMicros, Overflow uint64 }
  func (h *Histogram) Record(time.Duration); func (h *Histogram) Merge(*Histogram)
  func (h *Histogram) Quantile(q float64) time.Duration
  func NewArrivals(ratePerSecond float64, seed uint64) *Arrivals; func (a *Arrivals) Next() (time.Duration, bool)
  type Dispatcher struct{ Start time.Time; MaxInFlight int64; … }
  func (d *Dispatcher) Run(ctx context.Context, a *Arrivals, until time.Duration, op func(context.Context, time.Time))
  func (d *Dispatcher) Wait(); func (d *Dispatcher) Dropped() uint64; func (d *Dispatcher) Launched() uint64; func (d *Dispatcher) LagHistogram() Histogram
  type Recorder struct{…}; func NewRecorder() *Recorder
  func (r *Recorder) Done(route string, intended time.Time, outcome string)
  func (r *Recorder) Snapshot() map[string]RouteSummary
  type RouteSummary struct{ Latency Histogram; Outcomes map[string]uint64 }
  const OutcomeOK = "ok" // every other outcome string is unexpected
  type Model struct{ RefreshPerSecond, PullPerSecond, WritePerSecond, SnapshotPerSecond, MCPPerSecond, PushTokenPerSecond, LiveActivityPerSecond float64; LiveSockets int }
  func Model10k() Model; func (m Model) Scale(load float64) Model; func (m Model) Validate(devices, integrations int) error
  type CursorMix struct{ AtHead, SmallLag, LargeLag, FarBehind float64 }; func DefaultCursorMix() CursorMix
  type WriteMix struct{ Kinds []KindShare; Create, Update, Delete, MultiMutation float64 }; func DefaultWriteMix(withPump bool) WriteMix
  type Thresholds struct{ SyncP95, RefreshP95, MaxSchedulerLagP99 time.Duration; MaxUnexpectedRatio float64 }
  func SpecThresholds() Thresholds; func LoadThresholds(path string) (Thresholds, error)
  type RunSummary struct{ Routes map[string]RouteSummary; SchedulerLag Histogram; Dropped uint64; Audit AuditResult; Host HostVerdict; UnexpectedLiveLosses int }
  type AuditResult struct{ Acknowledged, Lost, CrossFamily, Diverged, TrackedDevices, TrackedDiverged int }
  type HostVerdict struct{ Contaminated bool; Offenders []string; MaxLoad1 float64 }
  type Verdict struct{ Pass bool; Reasons []string; Gates map[string]string }
  func Evaluate(s RunSummary, t Thresholds) Verdict
  type LedgerEntry struct{ FamilyID, EntityID, Kind string; Version uint64; Deleted bool; WireSHA256 string }
  func CreateLedger(path string) (*LedgerWriter, error); func (l *LedgerWriter) Append(LedgerEntry) error; func (l *LedgerWriter) Close() error
  func ReadLedger(path string) ([]LedgerEntry, error) // latest version per (family, entity)
  ```
- Route names (constants used by Tasks 10, 11 and 25): `RouteSyncPull = "sync_pull"`, `RouteSyncWrite = "sync_write"`, `RouteSyncHintPull = "sync_hint_pull"`, `RouteRefresh = "refresh_recoverable"`, `RouteRefreshAck = "refresh_ack"`, `RouteSnapshot = "snapshot_page"`, `RouteLive = "live_handshake"`, `RouteMCP = "mcp_call"`, `RoutePushToken = "push_token"`, `RouteLiveActivity = "live_activity"`. `SyncRoutes = []string{RouteSyncPull, RouteSyncWrite, RouteSyncHintPull}`.

- [ ] **Step 1: Write the failing tests.**

```go
package loadtest

func TestHistogramQuantilesWithinThreePercent(t *testing.T) {
	var h Histogram
	for ms := 1; ms <= 1000; ms++ { h.Record(time.Duration(ms) * time.Millisecond) }
	for _, c := range []struct{ q float64; want time.Duration }{{0.5, 500 * time.Millisecond}, {0.95, 950 * time.Millisecond}, {0.99, 990 * time.Millisecond}, {1, time.Second}} {
		got := h.Quantile(c.q)
		if got < c.want || float64(got) > float64(c.want)*1.032 {
			t.Errorf("q%.2f = %v, want %v..+3.2%%", c.q, got, c.want)
		}
	}
	var small Histogram
	small.Record(10 * time.Microsecond)
	if small.Quantile(1) != 10*time.Microsecond { t.Errorf("exact range: %v", small.Quantile(1)) }
}

func TestHistogramMergeAndOverflow(t *testing.T) {
	var a, b Histogram
	a.Record(time.Millisecond); b.Record(200 * time.Second)
	a.Merge(&b)
	if a.Total != 2 || a.Overflow != 1 || a.Quantile(1) != time.Duration(histMaxMicros)*time.Microsecond {
		t.Fatalf("merge: total %d overflow %d q1 %v", a.Total, a.Overflow, a.Quantile(1))
	}
}

func TestArrivalsArePoissonAtTheRate(t *testing.T) {
	a := NewArrivals(1000, 7)
	n := 0
	for { at, ok := a.Next(); if !ok || at >= time.Minute { break }; n++ }
	if n < 58200 || n > 61800 { t.Fatalf("%d arrivals in 60 s at 1000/s", n) }
	if _, ok := NewArrivals(0, 1).Next(); ok { t.Fatal("a zero rate arrived") }
}

func TestDispatcherNeverWaitsForSlowOperations(t *testing.T) {
	d := &Dispatcher{Start: time.Now(), MaxInFlight: 10_000}
	var started atomic.Int64
	d.Run(t.Context(), NewArrivals(200, 3), 2*time.Second, func(ctx context.Context, intended time.Time) {
		started.Add(1); time.Sleep(300 * time.Millisecond) // each op is slower than the gap
	})
	d.Wait()
	if n := started.Load(); n < 340 || n > 460 { t.Fatalf("launched %d, want ~400: the dispatcher waited", n) }
	lag := d.LagHistogram()
	// 50 ms, not the SLO's 20 ms: this runs under -race on a shared machine;
	// the property here is "never waits for an operation", not timer precision.
	if p99 := lag.Quantile(0.99); p99 > 50*time.Millisecond { t.Fatalf("scheduler lag p99 %v", p99) }
}

func TestDispatcherCountsDropsWhenSaturated(t *testing.T) {
	d := &Dispatcher{Start: time.Now(), MaxInFlight: 1}
	d.Run(t.Context(), NewArrivals(100, 5), time.Second, func(ctx context.Context, _ time.Time) { time.Sleep(200 * time.Millisecond) })
	d.Wait()
	if d.Dropped() == 0 { t.Fatal("saturation was absorbed instead of counted") }
}

func TestRecorderMeasuresFromIntendedStart(t *testing.T) {
	r := NewRecorder()
	r.Done(RouteSyncPull, time.Now().Add(-250*time.Millisecond), OutcomeOK) // intended 250 ms ago
	s := r.Snapshot()[RouteSyncPull]
	if got := s.Latency.Quantile(1); got < 250*time.Millisecond { t.Fatalf("latency %v measured from send, not intent", got) }
}

func TestModel10kAndScale(t *testing.T) {
	m := Model10k()
	if m.RefreshPerSecond != 70 || m.PullPerSecond != 600 || m.WritePerSecond != 10 || m.LiveSockets != 4000 {
		t.Fatalf("Model10k drifted from the spec: %+v", m)
	}
	x := m.Scale(2)
	if x.PullPerSecond != 1200 || x.LiveSockets != 8000 || x.WritePerSecond != 20 { t.Fatalf("Scale(2): %+v", x) }
	if err := x.Validate(40000, 200); err != nil { t.Fatalf("2x on 40,000 devices: %v", err) }
	if err := m.Scale(12).Validate(40000, 200); err == nil { t.Fatal("pulls above the device rate limit accepted") }
	if err := m.Validate(3000, 200); err == nil { t.Fatal("more sockets than devices accepted") }
}

func TestWriteMixSumsToOne(t *testing.T) {
	for _, pump := range []bool{false, true} {
		sum := 0.0
		for _, k := range DefaultWriteMix(pump).Kinds { sum += k.Share }
		if math.Abs(sum-1) > 1e-9 { t.Fatalf("pump=%v kinds sum %v", pump, sum) }
	}
}

func passing() RunSummary {
	fast := func() Histogram { var h Histogram; for i := 0; i < 1000; i++ { h.Record(40 * time.Millisecond) }; return h }
	routes := map[string]RouteSummary{}
	for _, r := range []string{RouteSyncPull, RouteSyncWrite, RouteRefresh, RouteRefreshAck} {
		routes[r] = RouteSummary{Latency: fast(), Outcomes: map[string]uint64{OutcomeOK: 1000}}
	}
	return RunSummary{Routes: routes, SchedulerLag: fast(), Audit: AuditResult{Acknowledged: 500, TrackedDevices: 200}}
}

func TestEvaluatePassesAndNamesEachBreach(t *testing.T) {
	if v := Evaluate(passing(), SpecThresholds()); !v.Pass { t.Fatalf("passing run failed: %v", v.Reasons) }
	slow := passing()
	h := Histogram{}; for i := 0; i < 1000; i++ { h.Record(400 * time.Millisecond) }
	slow.Routes[RouteSyncWrite] = RouteSummary{Latency: h, Outcomes: map[string]uint64{OutcomeOK: 1000}}
	expectReason(t, slow, "sync p95")
	errs := passing(); errs.Routes[RouteSyncPull].Outcomes["server_busy"] = 5 // 5 of 4005: > 0.1%
	expectReason(t, errs, "unexpected 0.12%")
	lost := passing(); lost.Audit.Lost = 1
	expectReason(t, lost, "lost 1")
	cross := passing(); cross.Audit.CrossFamily = 1
	expectReason(t, cross, "cross-family 1")
}

func TestEvaluateFailsSaturatedOrLaggingGenerator(t *testing.T) {
	s := passing(); s.Dropped = 3
	expectReason(t, s, "generator saturated: 3 dropped arrivals")
	l := passing(); l.SchedulerLag = Histogram{}; for i := 0; i < 100; i++ { l.SchedulerLag.Record(50 * time.Millisecond) }
	expectReason(t, l, "scheduler lag p99")
}

func TestEvaluateFailsContaminatedRun(t *testing.T) {
	s := passing(); s.Host = HostVerdict{Contaminated: true, Offenders: []string{"swift-frontend"}}
	expectReason(t, s, "host contaminated: swift-frontend")
}

func TestEvaluateFailsVacuousAudit(t *testing.T) {
	s := passing(); s.Audit.Acknowledged = 0
	expectReason(t, s, "audit vacuous")
}

func TestLedgerKeepsLatestVersionAndIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := CreateLedger(path)
	if err != nil { t.Fatal(err) }
	l.Append(LedgerEntry{FamilyID: "f", EntityID: "e", Kind: "feed", Version: 1, WireSHA256: "a"})
	l.Append(LedgerEntry{FamilyID: "f", EntityID: "e", Kind: "feed", Version: 2, WireSHA256: "b"})
	l.Close()
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 { t.Fatalf("mode %v", fi.Mode()) }
	entries, err := ReadLedger(path)
	if err != nil || len(entries) != 1 || entries[0].Version != 2 { t.Fatalf("%v %+v", err, entries) }
	if _, err := CreateLedger(path); err == nil { t.Fatal("an existing ledger was overwritten") }
}
```

`expectReason(t, s, substr)` asserts `!Evaluate(s, SpecThresholds()).Pass` and that some reason contains `substr`.

- [ ] **Step 2: Run, expect FAIL** (the package does not exist).

```bash
export GOCACHE=/path/to/bt-gocache-phase5-task3
cd Server && go test ./internal/loadtest -count=1
```

- [ ] **Step 3: Implement.**

`hist.go`:

```go
package loadtest

import (
	"math"
	"math/bits"
	"time"
)

// Latencies are recorded in microseconds from 0 to 2^27-1 (about 134 s):
// exact below 64 µs, then 32 buckets per power of two, so a reported
// quantile is at most 1/32 (about 3%) above the true value. Quantiles report
// a bucket's upper bound, never less than the true value (conservative).
const (
	histExact     = 64
	histPerOctave = 32
	histMaxMicros = 1<<27 - 1
	histBuckets   = histExact + 21*histPerOctave
)

type Histogram struct {
	Counts    [histBuckets]uint64 `json:"counts"`
	Total     uint64              `json:"total"`
	MaxMicros uint64              `json:"max_us"`
	Overflow  uint64              `json:"overflow"`
}

func bucketOf(us uint64) int {
	if us < histExact {
		return int(us)
	}
	if us > histMaxMicros {
		us = histMaxMicros
	}
	m := bits.Len64(us) - 6
	sub := us >> uint(m)
	return histExact + (m-1)*histPerOctave + int(sub-32)
}

func bucketUpper(i int) uint64 {
	if i < histExact {
		return uint64(i)
	}
	m := (i-histExact)/histPerOctave + 1
	sub := uint64((i-histExact)%histPerOctave + 32)
	return (sub+1)<<uint(m) - 1
}

func (h *Histogram) Record(d time.Duration) {
	us := uint64(max(d, 0) / time.Microsecond)
	if us > histMaxMicros {
		h.Overflow++
	}
	h.Counts[bucketOf(us)]++
	h.Total++
	h.MaxMicros = max(h.MaxMicros, min(us, histMaxMicros))
}

func (h *Histogram) Merge(o *Histogram) {
	for i, c := range o.Counts {
		h.Counts[i] += c
	}
	h.Total += o.Total
	h.Overflow += o.Overflow
	h.MaxMicros = max(h.MaxMicros, o.MaxMicros)
}

func (h *Histogram) Quantile(q float64) time.Duration {
	if h.Total == 0 {
		return 0
	}
	rank := max(uint64(math.Ceil(q*float64(h.Total))), 1)
	var seen uint64
	for i, c := range h.Counts {
		seen += c
		if seen >= rank {
			return time.Duration(min(bucketUpper(i), h.MaxMicros)) * time.Microsecond
		}
	}
	return time.Duration(h.MaxMicros) * time.Microsecond
}
```

`arrival.go`:

```go
// Arrivals is an open-loop Poisson process: each intended start is fixed in
// advance from the rate alone and never waits for an earlier operation.
type Arrivals struct {
	rate float64
	rng  *rand.Rand
	at   time.Duration
}

func NewArrivals(ratePerSecond float64, seed uint64) *Arrivals {
	return &Arrivals{rate: ratePerSecond, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

func (a *Arrivals) Next() (time.Duration, bool) {
	if a.rate <= 0 {
		return 0, false
	}
	a.at += time.Duration(a.rng.ExpFloat64() / a.rate * float64(time.Second))
	return a.at, true
}
```

`dispatcher.go`:

```go
// Dispatcher launches an operation at every intended start. It never waits
// for one to finish before launching the next: the open loop is what keeps a
// slow server from lowering the offered load (coordinated omission). When
// MaxInFlight operations are running, an arrival is dropped and counted,
// which fails the run (Evaluate), rather than delayed.
type Dispatcher struct {
	Start       time.Time
	MaxInFlight int64
	lagMu       sync.Mutex
	lag         Histogram
	inFlight    atomic.Int64
	dropped     atomic.Uint64
	launched    atomic.Uint64
	wg          sync.WaitGroup
}

func (d *Dispatcher) Run(ctx context.Context, a *Arrivals, until time.Duration, op func(context.Context, time.Time)) {
	for {
		offset, ok := a.Next()
		if !ok || offset >= until {
			return
		}
		intended := d.Start.Add(offset)
		if wait := time.Until(intended); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		} else if ctx.Err() != nil {
			return
		}
		d.lagMu.Lock()
		d.lag.Record(time.Since(intended))
		d.lagMu.Unlock()
		if d.inFlight.Load() >= d.MaxInFlight {
			d.dropped.Add(1)
			continue
		}
		d.inFlight.Add(1)
		d.launched.Add(1)
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			defer d.inFlight.Add(-1)
			op(ctx, intended)
		}()
	}
}

func (d *Dispatcher) Wait()            { d.wg.Wait() }
func (d *Dispatcher) Dropped() uint64  { return d.dropped.Load() }
func (d *Dispatcher) Launched() uint64 { return d.launched.Load() }
func (d *Dispatcher) LagHistogram() Histogram {
	d.lagMu.Lock()
	defer d.lagMu.Unlock()
	return d.lag
}
```

`recorder.go`: 64 shards keyed by `FNV(route) ^ goroutine-free counter`. Use `sync.Mutex` per shard and a round-robin `atomic.Uint64` to pick a shard. `Done` records `time.Since(intended)` into the shard's `RouteSummary` for the route. `Snapshot` merges the shards.

`model.go`:
- `Model10k()` returns the table's 1x values.
- `Scale` multiplies every rate and rounds `LiveSockets`.
- `Validate`:
  - `load > 0` is implied by positive rates;
  - `LiveSockets <= devices`;
  - pulls per device per minute ≤ 100 (`limits` default 120/min/device, so the model never trips it);
  - refreshes per device per 10 min ≤ 25 (`auth-refresh-device` 30/10 min);
  - `MCPPerSecond > 0` needs `integrations > 0`, and MCP per integration per minute ≤ 25 (quota 30/min).
- `DefaultCursorMix()` is {0.80, 0.15, 0.04, 0.01}.
- `DefaultWriteMix(withPump)` uses the table's shares. Without pump, feed is 35%.

`slo.go`:
- `SpecThresholds()` is `{SyncP95: 300ms, RefreshP95: 300ms, MaxSchedulerLagP99: 20ms, MaxUnexpectedRatio: 0.001}`.
- `Evaluate` merges `SyncRoutes` into one histogram for the sync gate and gates `RouteRefresh` and `RouteRefreshAck` separately. The unexpected ratio is the sum of non-`ok` outcomes over all outcomes, across all routes.
- It adds a reason for:
  - each breach;
  - `Dropped > 0`;
  - lag p99 above the bound;
  - contamination;
  - `Audit.Acknowledged == 0` ("audit vacuous");
  - any `Lost`, `CrossFamily`, `Diverged` or `TrackedDiverged` above zero;
  - `UnexpectedLiveLosses > 0`.
- `Gates` maps each gate to "value / threshold" for the report.
- `LoadThresholds` reads `thresholds.json` (Task 25 writes the tightened values). Until then the file holds the spec values.

`ledger.go`:
- The file is opened `O_CREATE|O_EXCL|O_WRONLY` with mode 0600, then written through a buffered writer under a mutex, one JSON line per entry.
- `ReadLedger` keeps the highest version per `(family, entity)`.

- [ ] **Step 4: Run green.**

```bash
cd Server && go test ./internal/loadtest -count=1 && go test -race ./internal/loadtest -count=3
```

Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
/usr/bin/git add Server/internal/loadtest
/usr/bin/git commit -m "feat(load): open-loop arrivals, intent-measured latency histograms, the 10k model, SLO verdicts and the write ledger"
```

---

### Task 4: The 10,000-family seeder and the fleet file

**Files:**
- Create: `Server/internal/loadtest/fleet.go` and `fleet_test.go` (untagged; the format is shared with Task 10)
- Create: `Server/internal/loadseed/{spec.go,ids.go,authority.go,care.go,side.go,backlog.go,verify.go,seed.go}` (`//go:build identitytest`)
- Create: `Server/internal/loadseed/{spec_test.go,care_test.go,seed_test.go}` (`//go:build identitytest`)
- Create: `Server/cmd/baby-tracker-load/main.go` (`//go:build identitytest`; this task adds `seed`, Tasks 10 and 11 add `run` and `audit`) and `main_disabled.go` (`//go:build !identitytest`; prints "baby-tracker-load needs -tags identitytest" and exits 2, as `cmd/sync-performance-client/main_disabled.go` does)

**Interfaces:**
- Produces (`loadtest/fleet.go`):

  ```go
  type DeviceState struct {
  	FamilyID, AccountID, DeviceID, Role string   // role: owner|caregiver|viewer
  	BabyIDs                             []string
  	AccessToken, RefreshToken           string
  	DeviceCredential                    string   // device continuity, for refresh-recoverable and ack
  	AccessExpiresAt, ReauthenticateAt   time.Time
  	Cursor                              uint64   // last acknowledged family sequence
  	ClientIP                            string   // 100.64.0.0/10, unique per device
  	PendingAttemptID                    string   `json:",omitempty"` // a refresh whose answer was lost
  	MCPCredential                       string   `json:",omitempty"`
  	LiveActivityToken, PushToken        string   `json:",omitempty"`
  }
  type FleetHeader struct{ Version int; SeedSpecSHA256 string; AsOf time.Time; Families, Devices int; ServerInstanceID string; RecoveryEpoch uint64 }
  func WriteFleet(path string, h FleetHeader, devices []DeviceState) error // atomic rename, 0600, refuses a non-private directory
  func ReadFleet(path string) (FleetHeader, []DeviceState, error)
  func (h FleetHeader) CheckFresh(now time.Time, lineageAge time.Duration) error // refuses now-AsOf > lineageAge - 24h
  ```
- Produces (`loadseed`):

  ```go
  type HistoryProfile struct{ DaysWeights map[int]float64; FeedsPerDay, DiapersPerDay, SleepsPerDay, SupplementsPerDay float64; PumpFamilyShare, PumpsPerDay, UpdateShare, TombstoneShare, RunningTimerShare, RunningPumpShare float64 }
  type Spec struct{ Families int; Seed uint64; AsOf time.Time; History HistoryProfile; BacklogHours int; BacklogLoad float64; MCPShare, PushTokenShare float64; ViewerShare, TwinShare float64; Workers int }
  func R10k(seed uint64, asOf time.Time) Spec
  func Small(families int, seed uint64, asOf time.Time) Spec // 7-day history; used by tests and the Task 12 smoke
  type Environment struct{ Config config.Config; Runtime, Superuser *pgxpool.Pool; Logger *slog.Logger }
  type Report struct{ Families, Accounts, Devices, CareEntities, Receipts, ConsumedSessions, LiveActivityRegistrations, PushTokens, MCPGrants int; DatabaseBytes int64; Phases map[string]time.Duration }
  func Seed(ctx context.Context, env Environment, spec Spec) (Report, loadtest.FleetHeader, []loadtest.DeviceState, error)
  ```
- Produces the CLI: `baby-tracker-load seed --secrets /run/load/private/secrets --families N --seed S --as-of RFC3339 --history r10k|small --backlog-hours 24 --backlog-load 2 --workers 16 --fleet /run/load/private/fleet.jsonl --report /run/load/seed-report.json`. It reads the same `BT_*_FILE` peppers as the app through `config.Load` with a getenv mapping the app's variables to files under `--secrets`, so every digest matches what the server computes.

- [ ] **Step 1: Failing tests.**

```go
// loadtest/fleet_test.go
func TestFleetRoundTripIsPrivateAndAtomic(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0o700)
	path := filepath.Join(dir, "fleet.jsonl")
	h := FleetHeader{Version: 1, AsOf: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), Families: 1, Devices: 1}
	in := []DeviceState{{FamilyID: "f", DeviceID: "d", AccessToken: "a", RefreshToken: "r", ClientIP: "100.64.0.1"}}
	if err := WriteFleet(path, h, in); err != nil { t.Fatal(err) }
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 { t.Fatalf("mode %v", fi.Mode()) }
	gotH, out, err := ReadFleet(path)
	if err != nil || gotH.Devices != 1 || !reflect.DeepEqual(in, out) { t.Fatalf("%v %+v", err, out) }
	os.Chmod(dir, 0o755)
	if err := WriteFleet(path, h, in); err == nil { t.Fatal("wrote credentials into a group-readable directory") }
}

func TestStaleSeedIsRefused(t *testing.T) {
	h := FleetHeader{AsOf: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	if err := h.CheckFresh(h.AsOf.Add(29*24*time.Hour+time.Hour), 30*24*time.Hour); err == nil {
		t.Fatal("a seed within 24 h of its lineage age was accepted")
	}
	if err := h.CheckFresh(h.AsOf.Add(48*time.Hour), 30*24*time.Hour); err != nil { t.Fatal(err) }
}
```

```go
//go:build identitytest
// loadseed/spec_test.go
func TestR10kShape(t *testing.T) {
	s := R10k(1, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	if s.Families != 10000 || s.BacklogHours != 24 || s.BacklogLoad != 2 { t.Fatalf("%+v", s) }
	if mean := s.History.MeanEntitiesPerFamily(s.TwinShare); mean < 950 || mean > 1250 {
		t.Fatalf("mean entities per family %.0f, want about 1,100", mean)
	}
}

func TestPlanIsDeterministicPerSeed(t *testing.T) {
	spec := Small(3, 42, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	a, b := planFamily(spec, 2), planFamily(spec, 2)
	if !reflect.DeepEqual(a, b) { t.Fatal("the same seed and index planned two different families") }
	if reflect.DeepEqual(a, planFamily(Small(3, 43, spec.AsOf), 2)) { t.Fatal("the seed is ignored") }
}
```

```go
//go:build identitytest
// loadseed/care_test.go: the rows Seed writes are the product's rows.
func TestSeededCareRowsEqualTheWritePathsRows(t *testing.T) {
	h := testkit.New(t)
	plan := planFamily(Small(1, 7, h.Now), 0)
	rows, state, err := encodeHistory(plan, h.FamilyID, accountsOf(h), devicesOf(h))
	if err != nil { t.Fatal(err) }
	// Apply the same canonical entities through the sync service on a second
	// harness, read both care_entities sets back, and compare column by column
	// (wire bytes, typed and derived columns, changed_seq order) and the
	// family counters (caretest.AssertCounters on the applied side; Seed's
	// computed FamilyState on the seeded side).
	assertSameRows(t, h, rows, state)
}
```

`assertSameRows` lives in the test file:
- it writes the seeded rows with `copyCareRows` into the first harness's database as the admin pool;
- it applies, through `h.Service`, the `protocol.Mutation`s that `plan` produces (`plan.Mutations()`) into a second `testkit.New(t)`;
- then it compares `SELECT kind, entity_version, wire, breast_seconds, bottle_volume_ml, started_at, ended_at, … FROM care_entities ORDER BY id`. Ignore `family_id`, `creator_account_id`, `actor_account_id`, `device_id`, `accepted_at` and the IDs inside the wire: the harnesses differ in IDs and clocks. For that, decode the wire into `protocol.CanonicalEntity` and blank `Meta`.

```go
//go:build identitytest
// loadseed/seed_test.go
func TestSeedSmallFamiliesEndToEnd(t *testing.T) {
	env := newSeedEnvironment(t) // testpg database at the current schema, server_state initialized, runtime + superuser pools, a config with fresh peppers
	report, header, fleet, err := Seed(t.Context(), env, Small(20, 1, env.Now()))
	if err != nil { t.Fatal(err) }
	if report.Families != 20 || len(fleet) != 80 || header.Devices != 80 { t.Fatalf("%+v", report) }
	// Every device authenticates through the product's request guard.
	for _, d := range fleet[:8] { mustAuthenticate(t, env, d.AccessToken, d.FamilyID) }
	// The audit chains verify, the counters hold and a pull from zero returns every row of the family.
	mustVerifyChains(t, env)
	for _, f := range distinctFamilies(fleet)[:3] { mustPullEverything(t, env, fleet, f) }
}
```

- [ ] **Step 2: Run, expect FAIL.**

```bash
export GOCACHE=/path/to/bt-gocache-phase5-task4
cd Server && go test ./internal/loadtest -run Fleet -count=1
BT_TEST_TAGS=identitytest BT_TEST_PACKAGES=./internal/loadseed bash scripts/test-db.sh
```

- [ ] **Step 3: Implement the seeder in stages.** `Seed` runs them in order. `Report.Phases` records each stage's time.

1. **`authority` (`authority.go`):** provisioning through the product's commands, `spec.Workers` families at a time.
   - Model: `internal/capacity/fixture_identitytest.go:165-350` (`composeFixture`, `provisionFixture`, `issueFixtureIntegration`). Compose the same services (`authority.New`, `family.New`, `identity.New`, `auth.New`, `familysync.New`) over `env.Runtime`, with a `testidentity.Issuer` as the Apple verifier and `config.ModeSelfHosted` for the sign-in path only.
   - Per family:
     - owner device 1: challenge → `issuer.Sign` → `Auth.Exchange` → `Families.Create`;
     - owner device 2: sign in;
     - caregiver: `Families.Invite("caregiver")`, then the caregiver signs in and runs `Families.Redeem` (viewer families: `Invite("viewer")`);
     - every device's bundle goes through `Auth.Refresh` to family scope;
     - `ExecuteOperatorEntitlement` grants `family_sync` with source `load_seed` (hosted requires it);
     - MCP grants for `MCPShare` of families.
   - Each device's `ClientIP` is `100.64.0.0 + global device index`.
2. **`care` (`care.go`):**
   - `planFamily(spec, i)`, deterministic from `rand.NewPCG(spec.Seed, uint64(i))`, plans babies, history days and entries. Care entity IDs come from `ids.go`: the first 16 bytes of SHA-256 of `seed‖family index‖ordinal`, shaped as UUIDv4.
   - `encodeHistory` builds each `protocol.CanonicalEntity` and calls `care.Encode(entity, previous, care.Change{Seq: n, ActorAccountID, DeviceID, Operation, AcceptedAt})` in sequence order. Updates encode the next version over the previous row; tombstones encode with `previous`. It accumulates `care.FamilyState`:
     - `Sequence` = the last seq;
     - `RecordCount` += `row.CountsAsRecord()`;
     - `ChargedBytes` += `row.ChargedBytes()`;
     - `EntityCount` and `WireBytes`, through `care.CounterDelta.Replace` exactly as the write path does (`internal/care/store.go:397-420`).
   - Timers and pumps that are running stay live with no end.
   - Load:
     - read the `care_entities_baby_fk` definition with `pg_get_constraintdef` and drop it;
     - load every family's rows with `pgx.CopyFrom` as the superuser (16 workers, one per hash partition, writing `care_entities_pNN` directly);
     - re-add the key under the same name and definition (one validation pass; the lesson of Phase 4 Task 22, `docs/verification/2026-10-09-legacy-import-scale.md` §1);
     - set `families.next_sequence` and the four `care_*` counters in one `UPDATE … FROM unnest(…)`.
   - Receipts: for rows accepted within 30 days of `AsOf`, insert `care_receipts` rows built the way the write path builds them (`internal/care/apply.go` around the receipt construction, and `care.ReceiptRow`). The receipt outcome is the `applied` outcome with the canonical entity.
3. **`side` (`side.go`):**
   - device cursors: `COPY device_sync_cursors` (80% at head, 20% lagging 1–20; `seen_at` within the last day);
   - push tokens through the product's push-token service (`internal/pushtoken`, the call the `PUT /v1/devices/push-token` handler makes) for `PushTokenShare` of devices;
   - a Live Activity registration for every device, plus an activity for families with a running timer, through the services the `PUT /v1/live-activities/…` handlers call (`internal/liveactivity`, `authority.WithLiveActivitySnapshot`);
   - tokens are synthetic 64-hex strings.
4. **`backlog` (`backlog.go`):** `COPY sessions` of `round(BacklogLoad × devices × BacklogHours × 6)` rows:
   - `status='consumed'`, with `consumed_at` uniform over `(AsOf − BacklogHours, AsOf)`;
   - random 32-byte `access_digest` and `refresh_digest`, a random `child_id`;
   - the device's real `account_id`, `device_id` and `lineage_root_id`; the live session's `scope`, `family_id` and `role`; the current `token_key_id`;
   - `issued_seq` 1, and expiry columns consistent with `sessions_check1`.

   The table has no self-foreign key (`000032_baseline.sql:7467-7475`), so these rows are valid. No audit rows are written for them; the report states it (D1).
5. **`verify` (`verify.go`):**
   - `authority.NewAuditVerifier(env.Superuser…).VerifyAll` is clean;
   - for 50 sampled families, the four counters equal `SUM` over `care_entities`, mirroring `caretest.AssertCounters`;
   - for 20 sampled devices, a `familysync` pull from cursor 0 paged to the end returns exactly the family's row count, and every entity's `meta.family_id` matches;
   - then `VACUUM (ANALYZE)` as the superuser, waiting until `pg_stat_progress_vacuum` is empty;
   - `DatabaseBytes` = `pg_database_size('babytracker')`.
6. The fleet is written with `FleetHeader{SeedSpecSHA256: sha256(json(spec)), AsOf, ServerInstanceID, RecoveryEpoch}`.

- [ ] **Step 4: Run green**, the Step 2 commands, then time the small seed:

```bash
cd Server && go test -tags identitytest ./internal/loadseed -run TestR10kShape -count=1
BT_TEST_TAGS=identitytest BT_TEST_RACE=0 BT_TEST_VERBOSE=1 BT_TEST_PACKAGES=./internal/loadseed BT_TEST_RUN=TestSeedSmallFamiliesEndToEnd bash scripts/test-db.sh
```

Expected: PASS. The test log shows each phase's time for 20 families.

- [ ] **Step 5: Commit.**

```bash
/usr/bin/git add Server/internal/loadtest/fleet.go Server/internal/loadtest/fleet_test.go Server/internal/loadseed Server/cmd/baby-tracker-load
/usr/bin/git commit -m "feat(load): the 10k-family seeder — authority through the product's commands, care history through care.Encode and COPY, side tables, session backlog, verified"
```

---

### Task 5: Property test — concurrent family and account commands never deadlock

What already exists, and is kept:
- `internal/sync/convergence_test.go` covers "a device that pulls in any order converges": `TestPullAndSnapshotConvergeUnderAnyInterleaving` (40 seeds × 120 steps) and `TestConcurrentWritersAndPagersConverge`.
- `internal/authority/locks_test.go:23` `TestLockOrderingNeverDeadlocks` exercises the lock primitives only.
- Pairwise command races exist: `family/integration_test.go:537-591`, `authority/sessions_test.go:638,820` and `sync/request_concurrency_test.go:30`.

The gap is a randomized property over the **real** commands.

**Files:**
- Create: `Server/internal/family/command_property_test.go`

**Interfaces:**
- Consumes: `testkit.New`, `h.AddMember`, `h.Refresh`, `h.Family` (`*family.Service`: `Invite`, `Redeem`, `ChangeRole`, `Remove`, `Leave`, `Transfer`, `UpdateProfile`, `RevokeInvitation`, `ScheduleDeletion`, `Recover`), `h.Auth` (refresh, logout), `h.Authority` (`DeleteAccount` via the account path, `ExecuteOperatorEntitlement`), and `authority.NewAuditVerifier(...).VerifyAll`.

- [ ] **Step 1: Write the test.**

```go
package family_test

// Property: 8 seeds × 160 commands, run 16 at a time over one family with 6
// members (owner, 3 caregivers, 2 viewers) and a second family sharing one
// account's other device. The commands are chosen at random from invite,
// redeem, change role, remove, leave, transfer ownership, profile update,
// revoke invitation, schedule deletion, recover, refresh, logout and
// entitlement grant. No command deadlocks (40P01) or waits out a lock
// timeout (55P03). Every other failure is one of the commands' documented
// refusals. Afterwards every audit chain verifies and the node invariants
// hold: one owner per active family, at most one active family per account,
// and session rows agree with memberships.
func TestRandomFamilyAndAccountCommandsNeverDeadlock(t *testing.T) {
	for seed := range uint64(8) {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			h := testkit.New(t)
			world := newCommandWorld(t, h, seed) // members, a second family, pending invitations
			rng := rand.New(rand.NewPCG(seed, 0x5eed))
			plan := world.plan(rng, 160)
			var deadlocks, timeouts, unexpected atomic.Int32
			sem := make(chan struct{}, 16)
			var wg sync.WaitGroup
			for _, cmd := range plan {
				wg.Add(1)
				sem <- struct{}{}
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					err := cmd.run(t.Context())
					var pgErr *pgconn.PgError
					switch {
					case err == nil, world.documentedRefusal(cmd, err):
					case errors.As(err, &pgErr) && pgErr.Code == "40P01":
						deadlocks.Add(1)
					case errors.As(err, &pgErr) && pgErr.Code == "55P03":
						timeouts.Add(1)
					default:
						unexpected.Add(1)
						t.Logf("%s: %v", cmd.name, err)
					}
				}()
			}
			wg.Wait()
			if deadlocks.Load()+timeouts.Load()+unexpected.Load() > 0 {
				t.Fatalf("deadlocks %d, lock timeouts %d, unexpected %d", deadlocks.Load(), timeouts.Load(), unexpected.Load())
			}
			world.assertInvariants(t) // VerifyAll clean; owners; slots; sessions match memberships
		})
	}
}
```

`documentedRefusal` accepts only the `authority` error codes the command's doc comment lists (`internal/authority/family.go:197-571` lists them per command: `owner_required`, `membership_revoked`, `invitation_consumed`, `caregiver_limit`, `entitlement_required`, `invalid_command` and so on), plus `ErrAuthorizationChanged`-style refusals for principals revoked by an earlier command. Matching is by `errors.Is` against the exported codes in `internal/authority/errors.go`.

- [ ] **Step 2: Run against today's code.**

```bash
export GOCACHE=/path/to/bt-gocache-phase5-task5
BT_TEST_PACKAGES=./internal/family BT_TEST_RUN=TestRandomFamilyAndAccountCommandsNeverDeadlock bash Server/scripts/test-db.sh
```

Expected: PASS (it pins today's lock order). If it FAILs:
- treat it as a real finding, using superpowers:systematic-debugging;
- reduce to the failing seed and two commands;
- fix the lock order in the command, in a separate commit with its own regression test;
- record the finding in `progress.md`.

- [ ] **Step 3: Mutation check.** In a scratch change, make `authority.LockFamilies` lock in the given order instead of ID order (`internal/authority/locks.go`), run Step 2 and expect FAIL with deadlocks > 0. Revert the scratch change.

- [ ] **Step 4: Commit.**

```bash
/usr/bin/git add Server/internal/family/command_property_test.go
/usr/bin/git commit -m "test(family): random concurrent family and account commands never deadlock and keep every chain and invariant"
```

---

### Task 6: Flaky tests and the deterministic wake tick counter

**Files:**
- Modify: `Server/scripts/test-db.sh` (`BT_TEST_COUNT`), `Server/scripts/test_db_test.go`
- Modify: `Server/internal/mcpapi/tools_test.go` (`TestHistoryFittingUsesTheCompleteEnvelope`, line 162) and/or the code it exercises
- Modify: `Server/internal/sync/family_lock_test.go` (`TestMutationWaitsOnTheFamilyFenceFirst`, line 105) and/or the code it exercises
- Modify: `Server/cmd/baby-tracker-server/apns_sink_identitytest.go` (adds `GET /__identitytest/wake` → `{"ticks": n}`), `apns_sink_identitytest_test.go`
- Modify: `Packages/BabyTracker/Tests/FamilySyncClientE2ETests/BackgroundWakePushE2ETests.swift`

**Interfaces:**
- Consumes: `wake.Worker.Totals().Ticks` (Task 2). If Task 2 is not yet integrated, add the counter here and reconcile at integration.
- Produces: the identitytest route `GET /__identitytest/wake` returning `{"ticks": <uint64>}`.

- [ ] **Step 1: Add a repeat knob to `test-db.sh`** (Modify: `Server/scripts/test-db.sh`). `test-db.sh` hard-codes `-count=1`. Add `BT_TEST_COUNT=N`, which replaces it with `-count=N`. It must be a positive integer, else exit 2; the default of 1 keeps today's behaviour. Add the case to `Server/scripts/test_db_test.go`: `BT_TEST_COUNT=3` puts `-count=3` in the recorded `go test` arguments, and `BT_TEST_COUNT=x` exits 2. Tasks 18 and 23 use this knob too. Commit: `test(db): BT_TEST_COUNT repeats a selected test`.

- [ ] **Step 2: Reproduce each flake** before touching it (superpowers:systematic-debugging).

```bash
export GOCACHE=/path/to/bt-gocache-phase5-task6
cd Server
BT_TEST_COUNT=300 BT_TEST_PACKAGES=./internal/mcpapi BT_TEST_RUN='^TestHistoryFittingUsesTheCompleteEnvelope$' bash scripts/test-db.sh
BT_TEST_COUNT=300 BT_TEST_PACKAGES=./internal/sync BT_TEST_RUN='^TestMutationWaitsOnTheFamilyFenceFirst$' bash scripts/test-db.sh
```

Run each both on an idle machine and beside a 4-CPU burn (`docker run --rm --cpus 4 postgres:17.6-alpine@sha256:ef257d85… sh -c 'for i in 1 2 3 4; do (while :; do :; done) & done; sleep 900'`), because the recorded failures happened under load.
- Record the failure rate and the exact failure output.
- If neither reproduces in 300 runs under burn, record "not reproduced (0/300 idle, 0/300 burn)". Then read the test for timing assumptions (sleeps, fixed deadlines, a shared HTTP client, an `EOF` from a server closed before the read) and fix those structurally.

- [ ] **Step 3: Fix each root cause.** Write the fix and a note in the test's comment naming the root cause. The usual causes, to check in this order:
- a fixed-duration sleep standing in for a condition (replace it with a wait on the condition, bounded by the test context);
- a goroutine racing the assertion;
- an HTTP server shut down while a response body is read.

Never raise a timeout as the fix. Rerun the Step 2 loops, burn included, and expect 0/300.

- [ ] **Step 4: The deterministic tick counter** (P3 Task 18 note).
- In `apns_sink_identitytest.go`, register `GET /__identitytest/wake` next to `/__identitytest/apns`. It answers `{"ticks": worker.Totals().Ticks}`; pass the worker in through the same composition hook the recorder uses.
- In `BackgroundWakePushE2ETests.swift`, replace the "no push before write" `sleep 3 s` (vacuous under load; the tick is 2 s) with: read `ticks`, then poll until `ticks >= before + 2` (bounded by the test's deadline), then assert that no push was recorded for this test's token.
- Go test: `TestWakeTickRouteReportsTheWorkersTicks`.
- The Swift change keeps the test count the same, so `EXPECTED_TESTS` is unchanged.

- [ ] **Step 5: Verify.**

```bash
cd Server && go test -tags identitytest ./cmd/baby-tracker-server -run 'WakeTick|APNSSink' -count=1
pgrep -fl xcodebuild || true    # must print nothing before a Swift run on this shared machine
BT_DISPOSABLE_PUSH=1 make server-protocol-e2e
```

Expected: PASS. The background-push proof line is present.

- [ ] **Step 6: Commit** each flake fix separately, then the tick counter.

```bash
/usr/bin/git commit -am "test(mcpapi): <root cause>; history fitting no longer races <x> (0/300 under a 4-CPU burn)"
/usr/bin/git commit -am "test(sync): <root cause>; the family fence test waits on the lock, not a clock (0/300 under burn)"
/usr/bin/git commit -am "test(e2e): the background-push E2E waits for two wake ticks by the worker's counter, not a sleep"
```

---

### Task 7: The iOS capacity-matrix harness on the new server

`sync-performance.sh` still reads the retired `security_projection_head` (lines 208 and 248). It waits for the retired `identitytest recovery cleanup completed` marker (line 186; `protocol_e2e_test.go:419` lists that marker as retired), and it proves maintenance with the retired `app.cleanup_refresh_recovery_receipts`.

**Files:**
- Modify: `Server/scripts/sync-performance.sh`, `Server/scripts/lib/performance_control.py` (only if it reads the same retired names), `Server/scripts/test-performance-runner.sh`

**Interfaces:**
- Produces: `BT_SYNC_PERF_PROBE=1 bash Server/scripts/sync-performance.sh --mode matrix|foreground|socket …`, working against the PostgreSQL authority; Task 26 uses it. `--total-seconds` for `--mode matrix` accepts 60–3600; other modes keep 60–1200.

- [ ] **Step 1: Failing resource-free test.** Add a case to `test-performance-runner.sh`: `grep -n 'security_projection_head\|identitytest recovery cleanup completed\|cleanup_refresh_recovery_receipts' Server/scripts/sync-performance.sh` must print nothing. Add the matrix budget case: `--mode matrix --total-seconds 3600` passes argument validation, while `--mode socket --total-seconds 3600` is refused. Run `bash Server/scripts/test-performance-runner.sh`. Expected: FAIL.

- [ ] **Step 2: Implement.**
- **Head reader** (line 208 and line 248): `SELECT count(*) FROM security_audit_events`. "Authority history" is now the node's audit rows. The matrix's 248/512/2048 cells keep their meaning as a regression check: latency must not grow with history.
- **Maintenance proof:** find a privilege the maintenance login has and the runtime login lacks in the current manifest:

  ```sql
  SELECT object_identity, privileges FROM schema_privilege_manifest m
  WHERE schema_version = (SELECT current_version FROM schema_compatibility WHERE singleton)
    AND principal = 'baby_tracker_maintenance'
    AND NOT EXISTS (SELECT 1 FROM schema_privilege_manifest r WHERE r.schema_version = m.schema_version
                    AND r.principal = 'baby_tracker_runtime' AND r.object_identity = m.object_identity AND r.privileges = m.privileges)
  ORDER BY 1 LIMIT 1;
  ```

  Pin the row it returns as a `has_table_privilege` (or `has_function_privilege`) pair for `baby_tracker_export_app` against `baby_tracker_app`, as the old check did.
- **Worker wait:** delete the `phase=recovery_worker` wait loop. Readiness is already proven by `curl …/readyz` on line 177.
- **Budget:** in the argument check, allow `--total-seconds` up to 3600 when `--mode matrix`. 27 cells × ≤ 116 s needs about 55 minutes.
- **Client build:** run `go vet -tags identitytest ./cmd/sync-performance-client` and fix any compile drift.

- [ ] **Step 3: Verify.**

```bash
bash Server/scripts/test-performance-runner.sh
pgrep -fl xcodebuild || true
BT_SYNC_PERF_PROBE=1 bash Server/scripts/sync-performance.sh --mode socket --duration 30 --total-seconds 300 --output /path/to/bt-phase5-perf-socket-$(date +%s)
```

Expected: the runner test passes; the socket run exits 0 with `status.json` `{"phase":"complete","status":0}`. The full matrix and foreground runs are Task 26, on an idle machine.

- [ ] **Step 4: Commit.**

```bash
/usr/bin/git add Server/scripts/sync-performance.sh Server/scripts/test-performance-runner.sh Server/scripts/lib/performance_control.py
/usr/bin/git commit -m "test(perf): the capacity matrix and foreground runs read the PostgreSQL authority (audit rows, current maintenance proof, a matrix budget of an hour)"
```

---

### Task 8: Fault-logging follow-ups the load runs depend on (C40)

**Files:**
- Modify: the fault writer and `describeCause` in `Server/internal/httpapi` (`grep -n "func describeCause\|func logServerFault" Server/internal/httpapi/*.go`), `admission`/readiness refusal sites, `internal/auth` `AccessDigests`, `internal/httpapi/live.go` and `internal/httpapi/account.go` (or the file holding `/v1/account`)
- Test: `Server/internal/httpapi/fault_log_test.go`

**Interfaces:**
- Produces fault lines with:
  - `cause_code` for sentinels whose message matches `^[a-z][a-z0-9_]{0,63}$`;
  - `cause_class` of `timed_out` (the server's own deadline) or `client_gone` (the request context cancelled by the peer);
  - a rate-limited refusal fault line (at most one per second per class) for 503 `server_busy`, `not_ready` and hosted-auth 503.

- [ ] **Step 1: Failing tests** in `fault_log_test.go`. Add rows to the existing logged-request table:

```go
{name: "sentinel code is named", cause: errors.New("family_lock_timeout"), wantField: `"cause_code":"family_lock_timeout"`},
{name: "free text sentinel stays a type", cause: errors.New("could not read config at /x"), forbid: "/x"},
{name: "server deadline is timed_out", ctx: deadlineExceededByServer(), wantField: `"cause_class":"timed_out"`},
{name: "peer cancel is client_gone", ctx: cancelledByPeer(), wantField: `"cause_class":"client_gone"`},
{name: "server_busy has a fault line once a second", route: "/v1/sync", admissionFull: true, repeat: 50, wantLines: 1, wantField: `"refusal":"server_busy"`},
{name: "AccessDigests failure carries its cause", accessDigestsErr: fmt.Errorf("decode: %w", errKeyring), wantField: `"cause_type"`},
```

`TestFaultLoggingNeverChangesTheResponse` (exists) must still pass byte-for-byte.

- [ ] **Step 2: Run, expect FAIL.**

```bash
export GOCACHE=/path/to/bt-gocache-phase5-task8
cd Server && go test ./internal/httpapi -run 'Fault' -count=1
```

- [ ] **Step 3: Implement.**
- `describeCause` adds `cause_code` only for the code-shaped pattern. It never logs any other message text: user data and paths stay out.
- Classify the context: `errors.Is(err, context.DeadlineExceeded)` with the request context still alive means `timed_out`; a request context cancelled means `client_gone`.
- Refusal fault lines use one `atomic.Int64` "last logged second" per class.
- Wrap with `%w` at the three named sites, so the existing cause walker sees them.

- [ ] **Step 4: Run green**, then `cd Server && go vet ./... && BT_TEST_PACKAGES=./internal/httpapi bash scripts/test-db.sh`.

- [ ] **Step 5: Commit.**

```bash
/usr/bin/git commit -am "fix(logging): fault lines name code-shaped sentinels, tell server timeouts from gone clients, and record 503 refusals at most once a second"
```

---

### Task 9: Small hygiene (C6, C9, C13, C20, C38)

**Files:**
- Modify: `Server/internal/config/sync.go` or `config.go` (`BT_BODY_READ_TIMEOUT`), `Server/internal/httpapi/body_gate.go:45`
- Modify: `Server/internal/database/pool.go` (`openConfigured`)
- Modify: the export job code (`grep -rn "expires_at\|accepted_at" Server/internal/export/*.go`)
- Test: `internal/config/sync_test.go`, `internal/httpapi/body_gate_test.go`, `internal/database/pool_test.go`, `internal/export/*_test.go`, `internal/maintenance/receipts_test.go` (or the ReceiptSweep test file), `internal/limits/memory_test.go`

**Interfaces:**
- Produces: `BT_BODY_READ_TIMEOUT`, a Go duration from 1s to 60s (default 5s; out of range or malformed fails startup). It replaces `defaultBodyReadTimeout` as the gate's configured value.
- Produces: request and maintenance pools get `ConnectTimeout = 5s` when the DSN sets none. The side pools keep 2 s.

- [ ] **Step 1: Failing tests** (one per item).

```go
func TestBodyReadTimeoutIsConfigurable(t *testing.T) {
	for raw, want := range map[string]time.Duration{"": 5 * time.Second, "12s": 12 * time.Second} {
		cfg, err := loadSyncForTest(map[string]string{"BT_BODY_READ_TIMEOUT": raw})
		if err != nil || cfg.BodyReadTimeout != want { t.Fatalf("%q: %v %v", raw, cfg.BodyReadTimeout, err) }
	}
	for _, bad := range []string{"0s", "61s", "5", "-1s"} {
		if _, err := loadSyncForTest(map[string]string{"BT_BODY_READ_TIMEOUT": bad}); err == nil { t.Fatalf("%q accepted", bad) }
	}
}

func TestRequestPoolsBoundTheirConnectAttempt(t *testing.T) {
	pool, err := database.OpenWithMax(t.Context(), "postgres://u:p@127.0.0.1:1/db?sslmode=disable", 4)
	if err != nil { t.Fatal(err) }
	defer pool.Close()
	if got := pool.Config().ConnConfig.ConnectTimeout; got != 5*time.Second { t.Fatalf("connect timeout %v", got) }
}

func TestReceiptSweepConvergesPastSixteenBadFamilies(t *testing.T) {
	// 17 and 33 families whose receipts all fail to release (the existing
	// "bad family" fixture of the receipt sweep tests): the sweep finishes
	// in ceil(n/16) passes, each pass deleting the good families' rows,
	// and never loops.
}

func TestFullPoolLogsOncePerWindowUnderConcurrency(t *testing.T) {
	// 64 goroutines refused by a full verified pool inside one window: the
	// memory store's compare-and-swap logs exactly one line.
}
```

For the export clocks: `TestExportJobTimesShareOneClock` asserts that a job's `accepted_at` and `expires_at` come from the same clock source. Read the code first: if `expires_at` is computed in SQL (`now()`) while `accepted_at` comes from Go's `Now`, move both to one side, the side the rest of export uses, and pin it.

- [ ] **Step 2: Run, expect FAIL.**

```bash
export GOCACHE=/path/to/bt-gocache-phase5-task9
cd Server && go test ./internal/config ./internal/limits -run 'BodyRead|FullPoolLogs' -count=1
BT_TEST_PACKAGES='./internal/database ./internal/export ./internal/maintenance ./internal/httpapi' bash scripts/test-db.sh
```

- [ ] **Step 3: Implement** each item minimally, then document `BT_BODY_READ_TIMEOUT` in `Server/README.md`'s configuration table.

- [ ] **Step 4: Run green** (the same commands).

- [ ] **Step 5: Commit** one commit per item, for example:

```bash
/usr/bin/git commit -am "feat(config): BT_BODY_READ_TIMEOUT (1s–60s, default 5s) for the bounded unauthenticated body read"
```

---

## Wave B (after Tasks 3 and 4 are integrated)

### Task 10: The device-mix generator (`baby-tracker-load run`)

**Files:**
- Create: `Server/internal/loadtest/{client.go,device.go,ops_refresh.go,ops_pull.go,ops_write.go,ops_snapshot.go,ops_live.go,ops_misc.go,run.go,scenario.go}`
- Test: `Server/internal/loadtest/{client_test.go,ops_test.go,run_test.go,scenario_test.go}` (against `httptest` fakes of the v1 routes)
- Modify: `Server/cmd/baby-tracker-load/main.go` (the `run` subcommand)

**Interfaces:**
- Consumes: Task 3's types (`Arrivals`, `Dispatcher`, `Recorder`, `Model`, `CursorMix`, `WriteMix`, `LedgerWriter`, route constants), Task 4's `ReadFleet`/`WriteFleet`/`DeviceState`/`FleetHeader.CheckFresh`, and `internal/protocol` types for every request and response.
- Produces:

  ```go
  type RunConfig struct {
  	Base, Host string               // http://172.31.250.10:8080, sync.load.test
  	Fleet, Ledger, Tracked, Out string  // /run/load/private/fleet.jsonl, ledger.jsonl, tracked.jsonl, /run/load/result.json
  	Scenario string; Load float64; Steps []float64 // ramp only
  	Warmup, Measure, StepDuration time.Duration
  	Seed uint64; TrackedDevices int     // 200
  	StormAt, RestartAt, RestartGrace time.Duration
  	WithPump bool                       // true when the server advertises kind 10
  	Thresholds Thresholds
  }
  func Run(ctx context.Context, cfg RunConfig) (RunSummary, error)
  ```

  `result.json` holds the `RunSummary` plus per-step summaries (ramp) and per-window summaries (storm and restart). `$RUN/window-start` is written when warm-up ends.
- Scenarios:
  - `soak`: warm-up, then a measurement window at one load;
  - `ramp`: warm-up at the first step's load, then steps of `StepDuration`, stopping at the first failing step plus one step;
  - `storm`: soak, plus at `StormAt` every live socket closed client-side, reconnecting by the iOS policy;
  - `restart`: soak; errors in `[RestartAt, RestartAt+RestartGrace]` are classed `restart_window`;
  - `mass-snapshot`: 1x background, plus 10% of devices starting snapshots over 5 minutes;
  - `hot-family`: 2x background, plus one seeded 10-device family writing 50/s;
  - `flood`: 2x background, plus the IP-flood and bad-bearer cells of Task 19;
  - `import`: 2x background, plus 20 imports of 2,000 records;
  - `export`: 2x background, plus 5 slow downloads at 100 KiB/s.

- [ ] **Step 1: Failing tests** (fakes built on `httptest.NewServer` with handlers that record requests).

```go
func TestEveryRequestCarriesTheDevicesForwardedAddressAndHost(t *testing.T) {
	seen := recordRequests(t)
	c := newClient(seen.URL, "sync.load.test")
	d := &device{DeviceState: DeviceState{ClientIP: "100.64.0.7", AccessToken: "a"}}
	c.pull(t.Context(), d, nil)
	r := seen.last()
	if r.Header.Get("X-Forwarded-For") != "100.64.0.7" || r.Host != "sync.load.test" ||
		r.Header.Get("X-Baby-Tracker-Protocol") != "v1" || r.Header.Get("Authorization") != "Bearer a" {
		t.Fatalf("headers %v host %q", r.Header, r.Host)
	}
}

func TestExpiredAccessIsRefreshedNeverRetriedBlind(t *testing.T) {
	// The fake answers 401 access_expired to a sync. The generator records one
	// unexpected outcome ("access_expired": the schedule should have refreshed
	// first), refreshes through refresh-recoverable and ack, and does not
	// resend the failed sync.
}

func TestRecoverableRefreshRetriesALostAnswerWithTheSameAttempt(t *testing.T) {
	// The fake commits the refresh but drops the first response (closes the
	// connection). The device keeps PendingAttemptID, retries with the same
	// attempt ID and the old parent, receives the committed child and acks it.
	// The fleet then holds the child's tokens and no PendingAttemptID.
}

func TestPullFollowsPagesAndRejectsAnotherFamilysEntity(t *testing.T) {
	// The fake returns two pages; the second contains an entity whose
	// meta.family_id is another family. The pull records outcome
	// "cross_family", and both pages are timed in sync_pull.
}

func TestWritesLedgerEveryAppliedOutcome(t *testing.T) {
	// A write of 2 mutations answered "applied" with canonical entities appends
	// 2 ledger entries, with WireSHA256 = sha256(json.Marshal(entity)); a
	// "rejected" outcome is unexpected and is not ledgered.
}

func TestLiveSocketReconnectsOnLifetimeCloseWithoutCountingALoss(t *testing.T) {
	// The fake hub closes with 1001 (going away) and, separately, at token
	// expiry; the device reconnects and the loss counter stays 0. A close with
	// 1008 outside a declared window counts one unexpected loss.
}

func TestStormReconnectsWithTheIOSBackoff(t *testing.T) {
	// The reconnect delay for attempt n is uniform over 0..min(2^(n-1), 60) s
	// (SyncRetryPolicy(base: 1, ceiling: 60), FamilySyncWebSocket.swift:84).
	// The first attempt is drawn from 0..1 s.
}

func TestSoakSchedulesTheModelRates(t *testing.T) {
	// Against a fake that answers instantly: 40 devices, Model10k scaled so
	// that 40 devices carry 1% of it, 20 s measure. Launched counts per class
	// are within ±10% of rate × duration, and window-start is written when
	// warm-up ends.
}
```

- [ ] **Step 2: Run, expect FAIL.**

```bash
export GOCACHE=/path/to/bt-gocache-phase5-task10
cd Server && go test ./internal/loadtest -count=1
```

- [ ] **Step 3: Implement.**

**Client (`client.go`).**
- One `http.Transport`: `MaxIdleConns 4096`, `MaxIdleConnsPerHost 4096`, `IdleConnTimeout 90s`, `ForceAttemptHTTP2 false`. The server speaks HTTP/1.1 behind Tailscale Serve in production.
- Request timeout 10 s.
- Headers per route, as `internal/capacity/workload.go:293-333` sets them:
  - `X-Baby-Tracker-Protocol: v1`;
  - `X-Baby-Tracker-Capabilities: records-v1` for sync and snapshot;
  - the refresh-recovery capability set for refresh routes (`protocol.RefreshRecoveryCapabilities()`);
  - `X-Forwarded-For` from the device;
  - `Host` from the config.
- Bodies come from `protocol` types; responses decode into `protocol` types with a 4 MiB limit.
- Each request's outcome is `ok` or a fixed-cardinality class:
  - `server_busy`, `not_ready`, `rate_limited`, `http_4xx_<code>` (bounded to the codes in `protocol`), `http_5xx`;
  - `transport`, `timeout`, `protocol`;
  - `rejected_mutation`, `cross_family`, `access_expired`, `restart_window`.

**Devices (`device.go`).** Each device holds its `DeviceState`, a mutex, its own created entities (`id, kind, version, babyID`) and its live connection. A deterministic shuffle by `Seed` designates:
- `LiveSockets` live devices;
- `TrackedDevices` tracked devices (an in-memory entity map seeded by one snapshot at warm-up; every applied page updates it; written to `tracked.jsonl` at the end).

**Refresh (`ops_refresh.go`).** Every device has a timer at `start + U(0, interval)`, then every `interval ± U(−30 s, +30 s)`, where `interval = devices / RefreshPerSecond(load)`.
- The timer fires `POST /v1/auth/refresh-recoverable` with a fresh `refresh_attempt_id` (stored in `PendingAttemptID` before sending), then `POST /v1/auth/refresh-ack`.
- On a transport failure it retries with the same attempt.
- On success it replaces the tokens and clears the attempt.
- A live device also reconnects its socket with the new token: it opens the new socket first, then closes the old one with 1000.
- Latency is measured from the timer's intended time.

**Pulls (`ops_pull.go`).** A `Dispatcher` at `PullPerSecond(load)` picks a uniform random device. The cursor class comes from `CursorMix`; the device's cursor is rewound by the drawn lag (never below 0). The pull pages until `next_page_token` is nil.
- Every page is one `sync_pull` request; its latency is measured from its own intended time. The first page uses the arrival's intended time; later pages use the previous page's completion, because paging is a client loop.
- Every change and snapshot entity is checked: `meta.family_id == device.FamilyID`.
- The cursor advances only from the final page.

**Writes (`ops_write.go`).** A `Dispatcher` at `WritePerSecond(load)` picks a uniform random non-viewer device.
- The mutation comes from `WriteMix` and is built with the `protocol` constructors in the shapes of `Server/api/fixtures/v1/*` (`testkit.MustMutation` reads those fixtures). Read them to shape payloads; timers use start/stop, pumps follow the pumping plan's payload, immunizations the kind-9 payload. Kind 10 only when `WithPump`.
- Each `applied` outcome is appended to the ledger.
- A failed write is retried once after 1 s with the **same** mutation IDs. That is the lost-acknowledgement retry the spec keeps: it must answer `applied` or `already_applied` and is ledgered once.

**Snapshots (`ops_snapshot.go`).** A `Dispatcher` at `SnapshotPerSecond(load)` picks a device of an account with no active snapshot. It pages `GET /v1/sync/snapshot` to the end (`snapshot_page`), then pulls from the snapshot's high water.

**Live (`ops_live.go`).**
- Designated devices dial `/v1/live` with `github.com/coder/websocket` and the bearer, timing `live_handshake`.
- A reader goroutine keeps pongs flowing (`Read` loop). On a hint it schedules an immediate pull (`sync_hint_pull`) and checks the hint's `family_id`.
- Closes: 1001 or token expiry is a planned renewal (reconnect after U(0, 1 s)); anything else outside a declared window is an unexpected loss.

**Other classes (`ops_misc.go`).** Push-token `PUT` (`protocol/push_token.go` body), Live Activity `PUT` (`protocol/live_activity.go`) and MCP tool calls (the call shape of `internal/capacity/mcp.go:174-220`, with the device's family integration credential), each with its own `Dispatcher`.

**Orchestration (`run.go`).**
- Check `FleetHeader.CheckFresh(now, 30*24h)` first.
- Warm-up ramp: each device starts at `U(0, Warmup)`. On start, it refreshes if access expires within 60 s, pulls from its cursor, and opens its socket if designated live.
- Write `window-start`.
- Start the dispatchers and the refresh timers for `Measure`, then drain in-flight work for at most 15 s.
- Checkpoint the fleet atomically every 60 s and at the end, so tokens survive for the next run.
- Write `result.json`.
- `MaxInFlight` is 20,000 per dispatcher.

**Scenarios (`scenario.go`).** Each scenario wraps `Run` with its extra schedule:
- **storm:** at `StormAt`, close every live socket client-side at once, reconnect each by `SyncRetryPolicy(base 1, ceiling 60)` with full jitter, and report time-to-99%-reconnected.
- **restart:** mark the window.
- **ramp:** iterate the steps and evaluate each step's window with `Evaluate`.

Add `run` to `cmd/baby-tracker-load/main.go` with flags mirroring `RunConfig`. The program exits 0 when the run completed, PASS or not; the verdict is in `result.json`. It exits non-zero only when the generator itself failed.

- [ ] **Step 4: Run green.**

```bash
cd Server && go test ./internal/loadtest -count=1 && go test -race ./internal/loadtest -count=2
go vet -tags identitytest ./cmd/baby-tracker-load
```

Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
/usr/bin/git add Server/internal/loadtest Server/cmd/baby-tracker-load
/usr/bin/git commit -m "feat(load): the device-mix generator — refresh-recoverable schedules, pulls at mixed cursors, ledgered writes, snapshots, idle live sockets, MCP, push and Live Activity calls, storms and ramps"
```

---

### Task 11: Post-run audit — lost, cross-family and diverged changes

**Files:**
- Create: `Server/internal/loadtest/audit.go`
- Test: `Server/internal/loadtest/audit_db_test.go` (needs a database; runs under `test-db.sh`)
- Modify: `Server/cmd/baby-tracker-load/main.go` (`audit` subcommand: `--superuser-url-file`, `--ledger`, `--tracked`, `--out /run/load/audit.json`, `--result /run/load/result.json` to fold the audit into the summary and re-evaluate)

**Interfaces:**
- Consumes: `ReadLedger`, `LedgerEntry`, `AuditResult`, `Evaluate`.
- Produces:

  ```go
  type TrackedDevice struct{ FamilyID, DeviceID string; Cursor uint64; Entities map[string]TrackedEntity }
  type TrackedEntity struct{ Version uint64; WireSHA256 string; Tombstone bool }
  func Audit(ctx context.Context, db *pgxpool.Pool, ledger []LedgerEntry, tracked []TrackedDevice) (AuditResult, error)
  ```

- [ ] **Step 1: Failing tests.**

```go
func TestAuditDetectsLostCrossFamilyAndDiverged(t *testing.T) {
	h := testkit.New(t)
	ledger := applyAndLedger(t, h, 6) // 6 real writes through h.Service; entries from their outcomes
	if r, err := Audit(t.Context(), h.Admin, ledger, nil); err != nil || r.Lost+r.CrossFamily+r.Diverged != 0 || r.Acknowledged != 6 {
		t.Fatalf("clean audit: %+v %v", r, err)
	}
	h.AuthorityExec(t, `DELETE FROM care_entities WHERE family_id=$1 AND id=$2`, ledger[0].FamilyID, ledger[0].EntityID)
	moved := ledger[1]; moved.FamilyID = h.OtherFamilyID(t)              // the row exists, under the true family
	changed := ledger[2]; changed.WireSHA256 = strings.Repeat("0", 64)    // same version, different wire
	r, err := Audit(t.Context(), h.Admin, []LedgerEntry{ledger[0], moved, changed, ledger[3]}, nil)
	if err != nil || r.Lost != 1 || r.CrossFamily != 1 || r.Diverged != 1 {
		t.Fatalf("got %+v %v; want 1 lost, 1 cross-family, 1 diverged", r, err)
	}
}

func TestLedgerHashEqualsStoredWire(t *testing.T) {
	// For each applied outcome, sha256(json.Marshal(outcome.CanonicalEntity))
	// equals sha256(care_entities.wire): the round trip the audit relies on.
}

func TestTrackedDevicesConvergeOrAreCounted(t *testing.T) {
	// A tracked device whose map matches every row with changed_seq <= cursor
	// is converged; changing one entity's version in its map counts
	// TrackedDiverged = 1.
}

func TestEmptyLedgerIsVacuous(t *testing.T) {
	r, _ := Audit(t.Context(), testkit.New(t).Admin, nil, nil)
	if v := Evaluate(RunSummary{Audit: r}, SpecThresholds()); v.Pass { t.Fatal("an empty ledger passed") }
}
```

`OtherFamilyID` creates a second family in the same harness. `testkit` already creates `Outsider`; use its family.

- [ ] **Step 2: Run, expect FAIL.**

```bash
export GOCACHE=/path/to/bt-gocache-phase5-task11
BT_TEST_PACKAGES=./internal/loadtest BT_TEST_RUN='Audit|LedgerHash|Tracked|EmptyLedger' bash Server/scripts/test-db.sh
```

- [ ] **Step 3: Implement.**
1. In batches of 1,000 entries: `SELECT e.family_id, e.id, ce.entity_version, sha256(ce.wire) FROM unnest($1::uuid[], $2::uuid[]) e(family_id, id) LEFT JOIN care_entities ce ON ce.family_id = e.family_id AND ce.id = e.id`. The join uses the primary key and partition pruning.
2. A missing row, or `entity_version < entry.Version`, is a candidate loss. For the candidates only, `SELECT family_id, id FROM care_entities WHERE id = ANY($1)` finds rows filed under another family: those are `CrossFamily`; the rest are `Lost`.
3. Equal version with a different hash is `Diverged`. A tombstoned entry compares the tombstone's version.
4. For tracked devices: `SELECT id, entity_version, sha256(wire), tombstoned_at IS NOT NULL FROM care_entities WHERE family_id = $1 AND changed_seq <= $2`. It must equal the device's map; any difference increments `TrackedDiverged`.

The `audit` subcommand reads `result.json`, sets `Audit`, re-runs `Evaluate` with the run's thresholds, and writes `audit.json` and `verdict.json`.

- [ ] **Step 4: Run green** (the same command). Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
/usr/bin/git add Server/internal/loadtest/audit.go Server/internal/loadtest/audit_db_test.go Server/cmd/baby-tracker-load/main.go
/usr/bin/git commit -m "feat(load): post-run audit of every acknowledged write and tracked device: zero lost, cross-family or diverged changes"
```

---

## Wave C (serial, on the node)

### Task 12: Pipeline smoke at 100 families

**Files:** script fixes only, if the pipeline exposes any; each fix is committed with its reason. Results go to `/path/to/bt-load/smoke-*`.

- [ ] **Step 1:** Run preflight and up.

```bash
export BT_LOAD_RUN=/path/to/bt-load/smoke-$(date +%Y%m%d-%H%M)
bash Server/scripts/load-node.sh preflight --families 100
bash Server/scripts/load-node.sh up --run "$BT_LOAD_RUN"
```

- [ ] **Step 2:** Seed, start and run.

```bash
bash Server/scripts/load-node.sh seed --run "$BT_LOAD_RUN" --families 100 --seed 1 --history small --backlog-load 1
bash Server/scripts/load-node.sh start --run "$BT_LOAD_RUN"
bash Server/scripts/load-node.sh run --run "$BT_LOAD_RUN" --scenario soak --load 1 --warmup 2m --minutes 5 --profile-at 2m
bash Server/scripts/load-node.sh audit --run "$BT_LOAD_RUN"
```

The load factor is per family: the generator already scales the 10k model to the fleet (100 families get 1% of every rate), so `--load 1` is 1x. (The first smoke ran `--load 0.01`, scaled twice: 0.3 expected writes, and the audit failed it as vacuous.)

- [ ] **Step 3: Pipeline proofs.** Each is checked and recorded in `progress.md`:
1. `verdict.json` has `Pass: true`, with non-zero acknowledged writes and tracked devices.
2. **XFF trust works.** In `app.log`, the IP-scoped limiter has 400 distinct keys. Read `limiter.ip_evictions` = 0, and confirm that no `rate_limited` outcome appears. Then a negative check: run `curl` from the host through the published pprof port's sibling, or `docker exec` into loadgen, sending 130 `refresh-recoverable` calls with a garbage token and a fixed `X-Forwarded-For` within 10 minutes. Expect 429 after 120 (`auth-refresh-ip`), which proves the server keys on the forwarded address.
3. `caps.json` is as declared; `profiles/` holds 5 files; `pg_stat_statements.json` is non-empty; `pg-locks.jsonl` and `docker-stats.jsonl` have one line per interval.
4. **Calibration.** `seed-report.json` gives `DatabaseBytes / CareEntities`. Project R10k's database size, and stop if the projection plus 15 GB of headroom exceeds `df` free.

- [ ] **Step 4:** `bash Server/scripts/load-node.sh down --run "$BT_LOAD_RUN"`. Commit any fixes:

```bash
/usr/bin/git commit -am "fix(load): <what the smoke exposed>"
```

---

### Task 13: Seed 10,000 families and take the first measurement

**Files:**
- Create: `docs/verification/<date>-phase5-load.md` (status "in progress"), with the sections "Reference node", "Seed", "First measurement" and "D1 inputs"
- Create: a draft of `docs/verification/<date>-phase5-load-evidence.json`

- [ ] **Step 1:** Run preflight and up for 10,000 families (`--families 10000` triggers the 30 GB disk floor).

- [ ] **Step 2:** Seed R10k and record each phase's duration and the database size.

```bash
bash Server/scripts/load-node.sh seed --run "$BT_LOAD_RUN" --families 10000 --seed 1 --history r10k --backlog-load 2
```

Target: ≤ 15 minutes in total. The hard limit is 30. Above 15, profile the seeder (`go tool pprof` on a `-cpuprofile` run of the 1,000-family shape) and fix the dominant cost in Task 4's code before measuring. Common fixes:
- more COPY workers;
- encoding on more goroutines;
- dropping and re-adding the secondary indexes of `care_entities` around the load, if the profile shows index maintenance.

Record the before and after times in the commit.

- [ ] **Step 3:** Start the app. Check that `app.log` has `runtime limits gomaxprocs=4` and that the verifier's first pass completes. Record its duration over about 50,000 scopes.

- [ ] **Step 4: First measurement at 1x.** `run --scenario soak --load 1 --warmup 10m --minutes 15 --profile-at 5m,12m`, then `audit`.

- [ ] **Step 5: First measurement at 2x.** `run --scenario soak --load 2 --warmup 5m --minutes 15 --profile-at 5m,12m`, then `audit`. The fleet is warm from Step 4.

- [ ] **Step 6: Write the baseline into the report.** It has:
- the per-route p50/p95/p99/max table at 1x and 2x;
- unexpected outcomes by class;
- the audit;
- app and db CPU and memory (mean and peak, from `docker-stats.jsonl`);
- app heap, goroutines and pool waits (from `app.log` samples);
- the `pg_stat_statements` top 10;
- blocked-lock counts and the wait-event mix;
- host load and the contamination verdict;
- the top 15 frames of each CPU profile (`go tool pprof -top -nodecount=15`);
- mutex and block top 10.

- [ ] **Step 7: D1 inputs.**
- Measured audit rows per second at 1x and 2x: the `security_audit_events` row-count delta over the window, split by kind.
- Bytes per row: `pg_total_relation_size` of the current month's partition divided by its rows.
- The projected GB per day and per month at 1x.
- The time to `DROP` a monthly partition of the projected size: measure on a synthetic partition built with `generate_series` to 10% of a month, and scale linearly.
- What the verifier needs to anchor at the oldest kept partition: read `internal/authority/verifier.go:297` `verifyScope`'s anchor handling.

Put these in the report's "D1 inputs" and in `progress.md`, and ask the owner for decision D1 (see "Owner decisions").

- [ ] **Step 8: Ranked hypotheses.** List the top five hypotheses from the profiles, each with the frames or query IDs that support it. These feed Tasks 14–21.

- [ ] **Step 9: Commit the docs.**

```bash
/usr/bin/git add docs/verification
/usr/bin/git commit -m "docs(verification): Phase 5 reference node, 10k seed and the first 1x/2x measurement"
```

Keep the volume for Wave D (`down --keep-volume`).

---

## Wave D (tuning — code in parallel worktrees; every measurement serial on the node)

Each task below follows the same loop:
1. **Measure** with the named cell. Use the first measurement when it already answers the question.
2. **Hypothesis**, written in the ledger.
3. **Change**, with tests.
4. **Re-measure** the same cell at the same load.
5. **Commit** with before/after numbers.

The A/B runs reuse the seeded volume. Each records its starting row counts, because tuning runs add rows; the drift is about 36,000 rows per 30-minute 2x run, which is negligible against 11M.

**Noise baseline (first, before any A/B):** run two identical 2x soaks of 15 minutes back to back on the unchanged code, and record the spread of every gate's value (for example, sync p95 412 vs 398 ms is a spread of 14 ms). A change counts as an improvement only when its delta exceeds that spread on its target metric.

### Task 14: Socket limit and memory budget (C3, C26, C30, C31, C46)

**Files:**
- Modify: `Server/internal/protocol/sync.go:145,191` (the `SocketProcessLimit` default and bounds), `Server/internal/config/sync.go:59`
- Create: `Server/cmd/baby-tracker-server/memory_limit.go` and test
- Modify: `main.go` (call it before any pool opens)
- Fixture: if `Server/api/fixtures/v1/*` or `docs/protocol/v1.md` pins `socket_process_limit: 1000`, update the value. Check `grep -rn socket_process_limit Packages/BabyTracker/Sources` first: the iOS client must not depend on it.

**Interfaces:**
- Produces: `applyMemoryLimit(getenv func(string) string, read func(string) ([]byte, error), logger *slog.Logger) int64`. When `GOMEMLIMIT` is unset and the cgroup `memory.max` is a number, it sets `debug.SetMemoryLimit(memoryMax * 85 / 100)` and returns it. Otherwise it changes nothing and returns `debug.SetMemoryLimit(-1)`.

- [ ] **Step 1: Measure per-connection memory.** Use the `soak` scenario with every class except live set to 0 (`--model-override live-only`; add that flag to `run` here).
- Sockets go 0 → 4,000 → 8,000 at `BT_LOAD_SOCKET_LIMIT=10000`, holding each level for 3 minutes.
- Heap profile at each level, plus the `runtime.HeapInuse` samples.
- Per-connection bytes = Δheap / Δconnections; split by package (`go tool pprof -top -inuse_space` filtered by `realtime` and `coder/websocket`).
- Also record:
  - the tracker size at 40,000 devices (`wake.tracker_entries` × the tracker's bytes per entry from the heap profile);
  - the limiter's live bytes at 2x (`limits` in the 2x heap profile);
  - the pgx statement cache per connection (`pgx` in the heap profile ÷ the runtime pool size).

- [ ] **Step 2: Budget table** in the report. Columns: component, at 2x on the reference node, and at the documented caps.

| Component | Count at caps |
|---|---|
| Hub | `BT_SOCKET_PROCESS_LIMIT` × per-connection bytes |
| Tracker | 200k bound × per-entry bytes (R2-M3: 66.2 MiB) |
| Limiter | verified + IP pools at caps (~180–192 MiB, P3 Task 1) |
| Request buffers | `BT_ADMISSION_TOTAL` × (1 MiB body + 1 MiB page) |
| pgx caches | total pool connections × per-connection cache |

Check the sum against 85% of 8 GiB (reference) and of 512 MiB (the homelab container).

- [ ] **Step 3: Failing tests.**

```go
func TestMemoryLimitDefaultsTo85PercentOfTheCgroup(t *testing.T) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(-1))
	got := applyMemoryLimit(func(string) string { return "" }, fakeCgroup("8589934592"), discard)
	if got != 8589934592*85/100 { t.Fatalf("limit %d", got) }
}
func TestExplicitGOMEMLIMITWins(t *testing.T) {
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(-1))
	before := debug.SetMemoryLimit(-1)
	got := applyMemoryLimit(func(k string) string { if k == "GOMEMLIMIT" { return "400MiB" }; return "" }, fakeCgroup("536870912"), discard)
	if got != before { t.Fatal("an explicit GOMEMLIMIT was overridden") }
}
func TestUnlimitedCgroupLeavesTheLimit(t *testing.T) { /* memory.max "max": unchanged */ }
func TestSocketProcessLimitDefaultAndBounds(t *testing.T) {
	if protocol.DefaultLimits().SocketProcessLimit != wantDefault { t.Fatal("default") }
	// accepts 65536, refuses 65537 and 9
}
```

- [ ] **Step 4: Implement.**
- `applyMemoryLimit` as specified.
- **The new socket default (the rule):** `wantDefault` = min(4,000, floor((0.85 × 512 MiB − the non-hub budget at the homelab's caps) / per-connection bytes)), rounded down to a multiple of 100.
  - If that is below 4,000, the default stays at the computed value, and the hosted profile (`Server/deploy/load/compose.yaml`, and Phase 6's operator docs) sets 10,000. Record the arithmetic in the commit.
  - Raise the maximum bound to 65,536 in both `protocol.Limits` validation and `config`, so the ramp (Task 24) can exceed 10,000.

- [ ] **Step 5: Re-measure.** The same live-only cell up to 12,000 sockets, with `BT_LOAD_SOCKET_LIMIT=20000`, and one 2x soak of 15 minutes. Both runs must show no heap growth beyond the budget and GC CPU < 5% of app CPU (from the CPU profile's `runtime.gcBgMarkWorker` share).

- [ ] **Step 6: Commit** the two changes separately, with the numbers:

```bash
/usr/bin/git commit -m "perf(runtime): GOMEMLIMIT defaults to 85% of the container's memory unless set" …
/usr/bin/git commit -m "perf(live): socket process limit default <n> (measured <b> bytes a connection), bound 65,536" …
```

---

### Task 15: Hub sweep under 8,000 sockets, and the reconnect storm (C29, C32)

**Files:**
- Modify: `Server/internal/realtime/hub.go:746-790` (`sweep`)
- Test: `Server/internal/realtime/hub_test.go`
- Modify: `Server/internal/diagnostics` (the sweep-duration histogram on the live sample)

**Interfaces:**
- Produces: `hubSweepChunk = 512`, and `LiveSample.SweepDurationNS Histogram`, `SweepClosed uint64` (sockets closed by an unavailable sweep).

- [ ] **Step 1: Measure.** From the 2x first measurement and a dedicated run at 8,000 and 12,000 sockets: the sweep duration distribution (add the observation first: time `CheckMany`) and any "authorization unavailable" closes in `app.log`. Decision rule:
- if `CheckMany` p99 > 1 s, or any sweep closed sockets, implement Step 2;
- otherwise record the numbers, skip Step 2, and keep the storm (Steps 3–4).

- [ ] **Step 2: Chunk the sweep** (when the rule fires).

```go
func TestSlowSweepClosesOnlyItsOwnChunk(t *testing.T) {
	// 1,200 leases; the authorizer answers chunk 2 (leases 512..1023) after the
	// sweep timeout and the others at once. Only chunk 2's 512 sockets are
	// closed ("authorization unavailable"); the other 688 stay open.
}
```

Implementation: `sweep` splits `items`/`leasing` into chunks of `hubSweepChunk`. Each chunk calls `CheckMany` under its own `context.WithTimeout(ctx, hub.sweepTimeout)`. A failed or late chunk detaches and closes only its own leases, failing closed with a bounded blast radius; denied leases close as today. Note in the doc comment that revocation still reaches sockets at once through `Invalidate`; the sweep is the backstop.

- [ ] **Step 3: The reconnect storm.** Run `run --scenario storm --load 2 --warmup 5m --minutes 10 --storm-at 4m`. Record:
- the time until 99% of the 8,000 sockets are reconnected;
- handshake p50/p95/p99 inside the storm minute;
- 429 and 503 counts by scope;
- sync p95 in the storm minute and in the minute after;
- app CPU peak;
- the PostgreSQL wait-event mix in the storm minute.

Pass (provisional, tightened in Task 25):
- 99% reconnected within 30 s;
- 0 unexpected losses after the storm minute;
- sync p95 ≤ 1 s in the storm minute and ≤ 300 ms from the next minute on.

If a limit refuses reconnects (for example `live-attempt-family` 120/min against a family's 4 devices), record it; that is expected to be fine. If admission or the pool is the bottleneck, that is a Task 16 input.

- [ ] **Step 4: Commit** the chunking (when done) with the before/after `CheckMany` numbers, and the storm results in the report.

```bash
/usr/bin/git commit -m "perf(live): the hub sweep re-checks leases in chunks of 512, so one slow answer closes 512 sockets, not all" …
```

---

### Task 16: Request pool, admission and PostgreSQL settings from measurement (C45, part of C24)

**Files:**
- Modify: the defaults in `Server/deploy/load/compose.yaml` (the documented hosted profile)
- Modify: `Server/README.md` ("Hosted profile (measured, Phase 5)" under the pool and admission settings)
- Modify, only if a code default changes: `internal/config` defaults and their tests

- [ ] **Step 1: Pool sweep.** At 2x, 10 minutes each, vary `BT_LOAD_RUNTIME_CONNS` over {16, 24, 32, 48} and `BT_LOAD_ADMISSION` over {32, 64, 96} (9 runs). Record:
- sync and refresh p95/p99;
- runtime-pool `wait_ns` per acquire (from the samples);
- `server_busy` count;
- db CPU and `pg_stat_activity` active counts.

- [ ] **Step 2: Pick values.** Choose the smallest pool and admission that keep p95 within 10% of the best observed, with zero `server_busy` at 2x, and that leave ≥ 25% of the cell's pool unused at p99 of acquired connections. Record the table and the choice in the report.

- [ ] **Step 3: PostgreSQL parameters.** Change a parameter only when the data names it:
- `temp_blks_written > 0` in `pg_stat_statements` → raise `work_mem` for that query class;
- shared-buffer hit ratio < 99% on `care_entities` partitions during a 2x run → raise `shared_buffers` to 3 GB, within the 8 GiB cap and decision D2;
- checkpoints more often than `checkpoint_timeout` (`pg_stat_bgwriter.checkpoints_req` growth) → raise `max_wal_size`.

Each change is one A/B and one commit (to the overlay).

- [ ] **Step 4: The maintenance pool (R2-M1).** Measure the wake tick delay while a family purge and an export run at 2x (`hot` maintenance). If the tick delay p99 is > 5 s with `BT_DB_MAINTENANCE_MAX_CONNS=4`, keep 4 or more in the hosted profile and implement the reserved wake slot in Task 18. Otherwise record that 4 suffices.

- [ ] **Step 5: Commit** the profile values, each with its before/after table.

```bash
/usr/bin/git commit -m "perf(config): hosted profile runtime pool <n>, admission <m> (measured at 2x: …)"
```

---

### Task 17: Retention throughput, AdaptiveBatch grow-back, verifier probes and the background connection (C7, C8, C18, C47)

**Files:**
- Modify: `Server/internal/maintenance/batch.go`, test `batch_test.go`
- Modify (only if Step 4 fires): `Server/internal/authority/verifier.go` (per-page partition probe) or `internal/database/pool.go` (`BackgroundConns`)

**Interfaces:**
- Produces: `AdaptiveBatch.GrowAfter int` (0 means 4). After `GrowAfter` consecutive full batches without a timeout, the size doubles, up to `Start`. A timeout resets the streak.

- [ ] **Step 1: Measure** at 2x with the seeded 2x backlog: per pass, rows pruned by `SessionSweep`, pass duration, batch size over time (log it), and the backlog of due rows (`count(*) FROM sessions WHERE status='consumed' AND consumed_at < now() - interval '24 hours'`, sampled each minute by the runner). Pass: the backlog does not grow over 15 minutes, given 140/s produced. Also from the 2x run, record:
- the verifier pass duration and its share of the 5-minute interval;
- the background pool acquire wait p99 (`SessionSweep` versus the verifier and key inventory).

- [ ] **Step 2: Failing test** (C18).

```go
func TestAdaptiveBatchGrowsBackAfterSuccesses(t *testing.T) {
	b := &AdaptiveBatch{Start: 1000, Min: 50, PassCap: 1 << 30}
	// statementTimedOut (batch.go:66) matches SQLSTATE 57014 while ctx is live.
	pgTimeout := func() error { return &pgconn.PgError{Code: "57014"} }
	b.Drain(t.Context(), func(ctx context.Context, limit int) (int, error) { return 0, pgTimeout() })
	if b.Size() != 500 { t.Fatalf("after a timeout %d", b.Size()) }
	full := 0
	b.Drain(t.Context(), func(ctx context.Context, limit int) (int, error) {
		full++
		if full > 12 { return 0, nil }
		return limit, nil
	})
	if b.Size() != 1000 { t.Fatalf("after 12 full batches the size is %d, want back at Start 1000", b.Size()) }
}
```

- [ ] **Step 3: Implement.** Add `streak atomic.Int64`. In `Drain`:
- on a timeout, halve as today and set `streak` to 0;
- on a full batch (`n == limit`), `if streak.Add(1) >= grow && limit < b.Start { b.size.Store(min(limit*2, b.Start)); streak.Store(0) }`.

- [ ] **Step 4: The conditional fixes.**
- **Verifier (C7):** when its pass is > 50% of its interval, or its partition probe is in the `pg_stat_statements` top 10, probe the partition once per scope page instead of per row/partition. Read `verifier.go` around `verifyScope` (line 297) and the page loop. Prove it with the existing `verifier_test.go` suite plus a test counting probe queries on a 3-partition fixture (expect 1 per page).
- **Background pool (C8):** when the background pool's acquire wait p99 is > 1 s, raise `database.BackgroundConns` to 2 and add it to the connection budget (`database.ValidateBudget`) and to Phase 6's operator notes.

- [ ] **Step 5: Re-measure** Step 1 and commit each change with the numbers.

```bash
/usr/bin/git commit -m "perf(maintenance): AdaptiveBatch grows back after four full batches (2x prune: … rows/s before, … after)"
```

---

### Task 18: Wake, Live Activity and APNs at 10,000 families (C15, C16, C17, C21, C24, C25, C35, C48, C55–C57)

**Files:**
- Modify: `Server/cmd/baby-tracker-server/apns_sink_identitytest.go` (`BT_IDENTITYTEST_APNS_SINK_LATENCY_MS`, 0–2000, default 0: a delay before the recorder answers)
- Modify: `Server/internal/wake/{worker.go,push.go}`, test `push_test.go`, `worker_test.go`
- Modify: `Server/internal/liveactivity` (only for a measured fix)
- Modify: `Server/internal/database/worker_slots.go` (a reserved wake slot, only when Task 16 Step 4 fired)

**Interfaces:**
- Produces: `wake.Worker.SendBudget time.Duration` (0 means 10 s). Sends run under `context.WithTimeout(context.WithoutCancel(tickCtx), SendBudget)`, so the tick deadline no longer cancels an in-flight push (C25).
- Produces: `applySettles` returns per-settle errors. A failing settle is quarantined (kept, retried next tick, counted in `Totals.SettlesFailed`) without blocking the others (C35).

- [ ] **Step 1: Measure** at 2x with `BT_LOAD_APNS_LATENCY_MS=100`, 15 minutes. Record:
- wake tick duration p50/p99, families per tick, pushes per second, NotSent and Ambiguous counts;
- duplicate pushes per token within 20 minutes (from the sink's records, sampled through `/__identitytest/apns` every minute; the recorder keeps 4,096);
- peak in-flight sends;
- the Live Activity minute-sweep duration over 40,000 registrations, claim latency, and change-feed flush lag;
- the hub-wake delay: the time from a family's commit to the hint at its live devices, measured by the generator as hint arrival minus the write ack.

- [ ] **Step 2: Failing tests** for the changes the measurement calls for. Always write:

```go
func TestTickDeadlineDoesNotCancelAnInFlightPush(t *testing.T) {
	// The tick context ends 50 ms into a push the sender answers after 200 ms:
	// the push completes and settles as delivered, not Ambiguous.
}

func TestOneFailingSettleDoesNotBlockTheOthers(t *testing.T) {
	// Three pending settles; the second's UPDATE fails: settles 1 and 3 are
	// applied, 2 stays pending, Totals.SettlesFailed == 1.
}
```

Conditional tests:
- **M6** (when the hub-wake delay p99 is > 2 s): `TestHubWakesAreNotDelayedBySlowSends`. With the sink at 500 ms, the next tick's hub wakes go out within one tick interval. The change sends outside the worker lock: collect the sends, release the lock, send, then settle.
- **M3/R4** (when push-due rows without a token are > 5% of scanned rows): `TestTokenlessDevicesAreNotRescanned`.
- **R3** (when a duplicate push is observed): `TestNoDuplicatePushAcrossTicksWithinTheWindow`.
- **m5** (at Task 24's first failing step): when high-UUID families' wake lag is > 3× the median, `TestScanRotatesItsStartUnderBacklog`.
- **R2-M1** (when Task 16 Step 4 fired): `TestWakeHasAReservedSlot`.

- [ ] **Step 3: Implement** each, re-measure Step 1, and commit separately with the numbers.

- [ ] **Step 4: Live Activity hardening under load (C57).**

```bash
docker run -d --rm --name bt-phase5-burn --cpus 4 postgres:17.6-alpine@sha256:ef257d85… sh -c 'for i in 1 2 3 4; do (while :; do :; done) & done; sleep 1800'
BT_TEST_COUNT=5 BT_TEST_PACKAGES=./internal/liveactivity bash Server/scripts/test-db.sh
docker rm -f bt-phase5-burn
```

Expected: PASS 5/5. Any failure is a finding and goes through systematic debugging.

- [ ] **Step 5: The APNs connection (C56).** Record the peak in-flight sends on the shared client. If it is > 100, add a client-side semaphore (`apns.Client.MaxInFlight`, default 100) with a test. Note in the report that Apple's own `SETTINGS_MAX_CONCURRENT_STREAMS` is honoured by Go's HTTP/2 transport and that the sink cannot reproduce it.

---

### Task 19: Limiter at 2x, IP flood, eviction scans and bad-bearer bodies (C2, C4, C12, C46)

**Files:**
- Modify: `Server/internal/loadtest/scenario.go` (the flood cells)
- Modify: `Server/internal/limits/memory.go` (only for a measured eviction fix), test `memory_test.go`

- [ ] **Step 1: Flood cells** (`--scenario flood --load 2 --minutes 15`), on top of 2x legitimate traffic:
- **IP flood:** 50,000 distinct `X-Forwarded-For` addresses from 198.18.0.0/15, each sending 1 `auth/challenge` and 1 refresh with a garbage token per minute (about 1,700 requests/s). This fills every IP scope pool.
- **Bad bearers:** 5,000 addresses each send 1 MiB `POST /v1/sync` bodies with random bearers at the `bearer-failure-ip` limit (60 per 10 minutes).

These are the generator's deliberate negative probes, counted in their own classes.

Measure:
- legitimate sync and refresh p95 against the plain 2x run;
- `limiter.ip_evictions` per scope;
- app CPU, and the CPU share of `limits` and of the body read in the profile;
- heap.

- [ ] **Step 2: Decision rules.**
- If eviction (`memoryShard.evict` and callees) is ≥ 2% of app CPU, implement sampled eviction: evict the soonest-ending of 16 random buckets instead of a full scan. Test with `TestEvictionIsSampledAndBounded`, which keeps the existing eviction-order tests.
- If legitimate p95 moves > 10% because of the bad-bearer cell, authenticate the bearer before reading the body (the order R1-I1's alternative named). That carries a refusal-precedence change, so pin it in the precedence test and in `docs/protocol/v1.md`. Otherwise record the CPU per refused body and keep the current order.

- [ ] **Step 3: Implement** what fired, re-measure, and commit with the numbers.

---

### Task 20: Measured-only carries (C14, C28, C49, C50, C51)

Each cell runs at 2x for 15 minutes on top of the normal mix. Each has a decision rule; a fix is its own commit with before/after numbers.

- [ ] **Step 1: HOT and fillfactor on `care_entities` (C49).**
  - **Measure:** `n_tup_upd`, `n_tup_hot_upd`, `n_dead_tup` and the autovacuum count per partition across a 2x run, and the index bloat after the run (`pgstattuple`'s `pgstatindex` is not installed; use `pg_relation_size` growth of each index).
  - **Expected:** an update changes `changed_seq` and the pull index covers it, so no update can be HOT whatever the fillfactor (Phase 2 :49). If so, record "no change" with the numbers.
  - **Rule:** change `fillfactor` or `autovacuum_*` for the partitions only if dead tuples exceed 20% of live tuples in any partition during the run. The change is codified in Task 22's migration.
- [ ] **Step 2: Hot family (C50).**
  - **Cell:** the `hot-family` scenario. One seeded family (seeder flag `--hot-family`: 5 accounts × 2 devices) writes 50/s, alongside 2x.
  - **Measure:** that family's write p95; other families' sync p95; runtime pool acquired connections; `pg_locks` waiters on `families` rows.
  - **Rule:** if other families' p95 moves > 10% or pool acquired connections hit the cap, give the family lock `SET LOCAL lock_timeout = '2s'`, answering a retryable 503 `family_busy`. That is a wire-visible code: check that `docs/protocol/v1.md` allows a retryable 503 on `/v1/sync` and that iOS `SyncRetryPolicy` retries 503. Pin it with a test that holds the family row in another transaction. Otherwise record "bounded by the pool, no change".
- [ ] **Step 3: MCP reads and history in one transaction (C51).**
  - **Measure:** MCP p95 and the round trips per MCP call (statement count per call from `pg_stat_statements` deltas over a 2x run with MCP at 2/s).
  - **Rule:** restructure only if an MCP call is ≥ 5% of db time, or MCP p95 > 300 ms.
- [ ] **Step 4: Export download inside the request transaction (C28).**
  - **Cell:** `export`. Five owners each create an export and download it at 100 KiB/s through a throttled reader.
  - **Measure:** runtime pool acquired connections over time; sync p95; and how long each download holds a connection.
  - **Rule:** if a download holds a request connection for longer than 10 s, or sync p95 moves > 10%, stream after the transaction. Read and verify the archive's manifest inside the transaction, release it, then stream the chunks with per-chunk re-authorization (`StatusPage`-style lock-free reads). Keep the "verifies the whole archive before the first byte" property (Phase 3 plan :262), pinned by the existing export tests plus `TestDownloadReleasesTheRequestConnectionBeforeStreaming`.
- [ ] **Step 5: Import resolve round trips (C14).**
  - **Cell:** `import`. 20 device imports of 2,000 records each during 2x.
  - **Measure:** resolve p95, statements per resolve, and the sync p95 delta.
  - **Rule:** batch the charged-bytes updates and remove the duplicated mappings read only if resolve p95 > 1 s or sync p95 moves > 10%. Prove it with a round-trip count test in `internal/importer` (the pattern of `testkit/roundtrips.go`).

---

### Task 21: The profile-driven tuning loop

Repeat until one of these stops holds, and record which stop ended the loop:
- (a) the 2x soak passes with p95 ≤ 210 ms on sync and refresh (30% headroom);
- (b) no single frame in the app CPU profile (flat) and no single query in `pg_stat_statements` exceeds 5% of its total, so no candidate is left;
- (c) six iterations.

- [ ] **Step 1:** Take the top candidate from the latest 2x profile: app CPU (flat and cumulative), mutex, block, `pg_stat_statements` `total_exec_time`, or the wait-event mix.
  - **Eligibility:** ≥ 5% of app CPU, ≥ 5% of db time, or a lock or wait class ≥ 5% of active samples.
  - **Micro-optimisations** (encoders, compression, encodings) are eligible only under the same rule; that is the spec's rule.
- [ ] **Step 2:** Write the hypothesis, with the evidence, in `progress.md`.
- [ ] **Step 3:** Change it in a worktree, with a test that pins behaviour. A performance-only change still keeps its package's tests green, plus `server-test`.
- [ ] **Step 4:** Run an A/B on the node: the same seed volume, 2x, 15 minutes, before and after.
- [ ] **Step 5:** Commit if the target metric improved by more than the Wave D noise baseline; otherwise revert. Log the iteration in the report's tuning log either way.

---

### Task 22: The one Phase 5 migration (C41, plus any codified tuning DDL)

**Files:**
- Create: `Server/migrations/0000NN_family_delete_cascade.sql`, with NN by the rule below
- Modify: `Server/internal/migrate/compatibility.json` (generated), the catalog golden (`catalog-NN.golden`), and the tests that pin the migration list (`migrate/compatibility_test.go`, `migrate/apply_history_test.go`, `care/entity_test.go` only if the care kinds change; the pumping plan's Task 2 Step 6 lists the same set)
- Test: `Server/internal/database/family_delete_cascade_migration_test.go`

- [ ] **Step 1: The numbering rule**, run when this task starts.

```bash
/usr/bin/git ls-tree --name-only main Server/migrations/ | grep -E '^Server/migrations/[0-9]{6}_' | sort | tail -1
for b in $(/usr/bin/git for-each-ref --format='%(refname:short)' refs/heads); do /usr/bin/git ls-tree --name-only "$b" Server/migrations/ 2>/dev/null; done | grep -E '[0-9]{6}_' | sort -u
```

1. If `main` has migrations this branch lacks, merge `main` into the branch first (as in Step 0), then rerun.
2. NN = the highest number on `main` + 1. On 2026-10-16 `main` has `000034_pumping.sql`, so NN = **000035**.
3. If another local branch already holds NN unmerged, keep NN anyway, because a contiguous chain needs it. Record in `progress.md` and the report that whichever branch merges second renames its file to the next number.

The rename procedure (for the owner-approved merge, whichever branch is second):
- `git mv`, then update the file's header comment and `introduced_by`;
- regenerate the manifest with `go run ./cmd/baby-tracker-manifest --migrations migrations --previous internal/migrate/compatibility.json --output internal/migrate/compatibility.json --min NN --max NN`;
- regenerate the golden (`BT_GOLDEN_UPDATE=1`);
- rerun `make server-test`.

- [ ] **Step 2: The failing test.**

```go
func TestRawFamilyDeleteCascadesThroughImportMappings(t *testing.T) {
	// A family with an import session, a shared-baby mapping and care rows.
	// As the owner (migrator) role: DELETE FROM families WHERE id = $1
	// succeeds whatever the trigger-name order, and no import_* row of the
	// family remains. Before the migration it depends on
	// import_mappings_shared_baby_fk firing after the cascade (Phase 4 Task 26 M4).
}
```

Also `TestMigrationNNKeepsEverySeededRow`: migrate a database at NN−1 with rows to NN, and assert the row counts are unchanged. This follows the immunizations migration test.

- [ ] **Step 3: Write the migration.** Read the current definition first, with `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'import_mappings_shared_baby_fk'`. Then:

```sql
-- 0000NN_family_delete_cascade: a raw DELETE FROM families no longer depends
-- on trigger-name order: import_mappings_shared_baby_fk cascades like the
-- other family-owned import rows (Phase 4 Task 26 M4). Plus the tuning DDL
-- Phase 5 measured (listed below, each with its run IDs), if any.
ALTER TABLE public.import_baby_mappings DROP CONSTRAINT import_mappings_shared_baby_fk;
ALTER TABLE public.import_baby_mappings ADD CONSTRAINT import_mappings_shared_baby_fk <the definition read above> ON DELETE CASCADE;
-- <codified tuning DDL from Tasks 16/20, if any>
INSERT INTO public.schema_privilege_manifest(schema_version,principal,object_kind,object_identity,privileges)
 SELECT NN,principal,object_kind,object_identity,privileges
 FROM public.schema_privilege_manifest WHERE schema_version=NN-1;
UPDATE public.schema_compatibility SET current_version=NN,
 introduced_by='0000NN_family_delete_cascade.sql',recorded_at=now() WHERE singleton;
```

Write the literal numbers in place of `NN`. Then regenerate the manifest (`--min NN --max NN`, the 000033 and 000034 convention) and the golden.

- [ ] **Step 4: Run green.**

```bash
export GOCACHE=/path/to/bt-gocache-phase5-task22
make server-test && make server-smoke
```

- [ ] **Step 5: Commit.**

```bash
/usr/bin/git commit -m "fix(schema): 0000NN family-owned import mappings cascade on a raw family delete"
```

---

## Wave E (serial runs)

### Task 23: Graceful restart under load and the mass snapshot (C42, C52, C58)

**Files:**
- Modify: `Server/internal/wake/worker.go` (`Run`: flush the tracker once more when stopping, within a 2 s budget, as `applySettlesOnStop` does), test `worker_run_test.go`

- [ ] **Step 1: Failing test (C42).**

```go
func TestRunFlushesTrackerProgressOnStop(t *testing.T) {
	// The tracker records progress for 3 devices after the last tick; cancel
	// Run. device_sync_cursors holds all 3 before Run returns.
}
```

Implement: on `ctx.Done()`, run one flush-only transaction (`Tick`'s flush statement without scans or sends) with `context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)`, after `applySettlesOnStop`. Run the test green and commit:

```bash
/usr/bin/git commit -m "fix(wake): flush device progress when the worker stops, so a graceful restart loses none"
```

- [ ] **Step 2: The restart cell.** Run `run --scenario restart --load 2 --warmup 5m --minutes 20 --restart-at 8m --restart-grace 90s`. The runner, at 8 minutes:
1. runs `docker compose stop -t 30 app` (SIGTERM, then a graceful stop) and immediately `start app`;
2. records the stop and ready times.

Record:
- the unavailability window (last 200 before the stop, first 200 after it);
- the requests failed inside the window, by class;
- the reconnect storm of 8,000 sockets (time to 99%);
- the time until sync p95 is back under 300 ms;
- the minutes after the restart: the wake startup keyset backstop over 40,000 cursors (duration, rows and db time), the Live Activity dirty sweep and first claims, and the verifier's first pass;
- the audit: zero lost writes. Writes in flight at SIGTERM were retried with the same mutation ID and answered `applied` or `already_applied`.

Pass:
- no unexpected outcome outside the window;
- the window ≤ 15 s;
- 99% of sockets back within 60 s of ready;
- zero lost, cross-family or diverged changes.

- [ ] **Step 3: Mass snapshot (C52).** Run `run --scenario mass-snapshot --load 1 --minutes 15`: 4,000 devices (10%) start snapshots spread over 5 minutes, with distinct accounts, so the account limit of 2 never refuses. Record:
- snapshot page p50/p95/p99;
- background sync p95 during the 5 minutes;
- db CPU and memory;
- the `sync_snapshot_sessions` row count;
- any `snapshot_limit` refusals.

Pass: background sync p95 ≤ 300 ms and unexpected outcomes < 0.1%.

- [ ] **Step 4:** Write both cells into the report and commit the docs.

---

### Task 24: The ceiling ramp

- [ ] **Step 1:** Run `run --scenario ramp --steps 1,1.5,2,2.5,3,3.5,4,5,6 --step-duration 5m --warmup 10m`, with `BT_LOAD_SOCKET_LIMIT=30000` (Task 14 raised the bound). Profile at the last passing step and at the first failing step.
- [ ] **Step 2:** The ceiling is the highest step whose own window passes every gate. Confirm it with a 10-minute soak at that load. If the confirmation fails, the ceiling is the next lower step, also confirmed.
- [ ] **Step 3:** State the ceiling in the report as a multiple of the model and in absolute rates (pulls/s, refreshes/s, writes/s, sockets). Name the limiting resource at the first failing step: app CPU, db CPU, a pool, a lock, the generator, or memory, with the evidence.
- [ ] **Step 4:** Check overload behaviour at the first failing step:
  - errors are `server_busy` (503) and not timeouts or crashes;
  - the app does not OOM (`docker inspect` `OOMKilled=false`);
  - the wake scan's lag for high-UUID families is ≤ 3× the median (C15; Task 18's conditional test applies otherwise).
- [ ] **Step 5:** Commit the report section.

---

### Task 25: The 30-minute soak at 2x (PASS) and the tightened thresholds (C59)

- [ ] **Step 1: Fresh dataset.** Run `down` (volumes too), `up`, `seed` (R10k, `--seed 1`, `--backlog-load 2`) and `start`. PASS runs on a pristine dataset at the final code.
- [ ] **Step 2: The soak.** Run `run --scenario soak --load 2 --warmup 10m --minutes 30 --profile-at 5m,15m,25m`, then `audit`.
- [ ] **Step 3: PASS** means `verdict.json` `Pass: true` on the spec thresholds. If it fails:
  - stop; the reason goes into the report;
  - fix through Task 21's loop;
  - then rerun Steps 1–2.

  The report keeps every failed soak.
- [ ] **Step 4: Tightened thresholds.** Write `Server/internal/loadtest/thresholds.json`:
  - sync and refresh p95 = min(300 ms, the soak's p95 × 1.5, rounded up to 10 ms);
  - unexpected ratio = min(0.001, max(0.0001, 2 × the measured ratio));
  - scheduler lag p99 stays 20 ms;
  - storm: 99% reconnected within max(10 s, 2 × measured) and storm-minute sync p95 ≤ 2 × measured;
  - restart: window ≤ max(10 s, 1.5 × measured).

  Add `TestThresholdsAreTighterThanTheSpec` in `slo_test.go`: every value is ≤ the spec's, and `LoadThresholds` reads the file. Commit:

```bash
/usr/bin/git commit -m "test(load): thresholds tightened after the first PASS (sync p95 ≤ <x> ms, refresh p95 ≤ <y> ms, unexpected ≤ <z>%)"
```

- [ ] **Step 5: Identity evidence.** Record:
  - `go list -f '{{.ImportPath}} {{.GoFiles}}'` for every request-path package (`httpapi`, `sync`, `care`, `authority`, `auth`, `realtime`, `wake`, `limits`, `admission`, `database`), with and without `-tags identitytest`;
  - the diff, which lists only `*_identitytest.go` files;
  - the image ID, the binary SHA-256 and the source SHA.

---

### Task 26: The iOS E2E capacity matrix and foreground latency against the new server (C43)

The matrix runs on `sync-performance.sh`'s own disposable server, not on the 10,000-family node. Its cells provision families through the real app's sign-in and invitation flow, and running it next to the loaded node would contaminate both measurements.

- [ ] **Step 1:** `pgrep -fl xcodebuild` prints nothing, and `load-node.sh preflight --families 100` passes (the idle check).
- [ ] **Step 2:** Run the matrix.

```bash
BT_SYNC_PERF_PROBE=1 bash Server/scripts/sync-performance.sh --mode matrix --duration 30 --total-seconds 3600 --output /path/to/bt-phase5-matrix-$(date +%Y%m%d-%H%M)
```

Expected: `status.json` reports `complete`. `cells.json` holds 27 cells; each completed cell is `coherent`. The audit-row (events) axis 248/512/2048 shows no latency growth: the 2048 cells' p95 is within 20% of the 248 cells' at equal families and concurrency.
- [ ] **Step 3:** Run foreground latency.

```bash
BT_SYNC_PERF_PROBE=1 bash Server/scripts/sync-performance.sh --mode foreground --duration 30 --total-seconds 1200 --output /path/to/bt-phase5-foreground-$(date +%Y%m%d-%H%M)
```

Expected: the healthy distribution's durable and observable p95 are < 1 s (the existing gate), and every proof line is present.
- [ ] **Step 4:** Run `make server-protocol-e2e` (two-caregiver, owner reinstall) and `BT_RECOVERY_E2E_MODE=self_hosted make server-protocol-e2e`. One rerun is allowed for the recorded Swift relaunch flake; a second failure blocks.
- [ ] **Step 5:** Write the matrix and foreground results into the report and commit.

---

### Task 27: D1 audit retention (gated on the owner's decision)

This task waits for decision D1. Implement the chosen option, test-first:
- **(a) or (b), time retention:**
  - a retention sweep drops whole partitions older than the kept window (`DROP TABLE` of a detached partition, `lock_timeout` 1 s, advisory-locked, as the partition roll is);
  - the verifier anchors at the oldest kept partition: a checkpoint row records the first kept row's `prev_hash` as the trusted anchor;
  - for daily retention under 60 days, switch the partition roll to daily partitions in the migration.

  Tests:
  - `TestRetentionDropsOnlyExpiredPartitions`;
  - `TestVerifierAnchorsAtTheOldestKeptPartition` (verify clean after a drop; a tampered row in a kept partition still fails closed);
  - `TestRetentionNeverDropsTheCurrentOrNextPartition`.
- **(c), no audit row for routine rotation:**
  - this is a spec change; the owner amends the spec text first;
  - refresh writes no account-scope audit row; reuse detection, revocation, sign-in and the rest keep theirs;
  - the refresh receipt and session rows remain the record.

  Tests:
  - `TestRefreshAppendsNoAuditRow`;
  - `TestRefreshReuseStillAppendsAnAuditRow`;
  - the existing chain tests stay green.

Re-measure the audit rows/s and the projected volume at 2x, and record before/after in the report. If the change touches the schema, it shares Task 22's migration when Task 22 has not shipped, or takes the next number by Task 22's rule.

---

### Task 28: The report, carries close-out and final gates

**Files:**
- Finish: `docs/verification/<date>-phase5-load.md` (the date is the PASS soak's) and `docs/verification/<date>-phase5-load-evidence.json`
- Modify: `.superpowers/sdd/2026-10-16-postgres-authority-phase5/progress.md` (git-excluded)

- [ ] **Step 1: The report's sections.**
  1. **Verdict:** PASS or FAIL at 2x for 30 minutes on "Apple M4, Docker CPU caps".
  2. **The declared node:**
     - host facts: CPU, memory, macOS, OrbStack version and `memory_mib` (decision D2);
     - caps and their proof (`caps.json`, the burn bound);
     - PostgreSQL settings, app settings, pools and admission;
     - images and SHAs;
     - the idle check and host load per run.
  3. **The model and the mix:** both tables, the sign-in exclusion, and the cursor and kind mixes.
  4. **The seed:** R10k's shape, measured row counts, the database size, phase times (target ≤ 15 minutes), and the audit-history statement.
  5. **Results:**
     - the PASS soak: route table (p50/p95/p99/max), unexpected outcomes by class, the audit (acknowledged, lost, cross-family, diverged, tracked devices), resources, profiles' top frames, `pg_stat_statements` top 10, and locks and waits;
     - the first measurement for comparison.
  6. **The measured ceiling** and its limiting resource.
  7. **Scenarios:** reconnect storm, graceful restart, mass snapshot, hot family, flood, import and export cells.
  8. **The tuning log:** every hypothesis, change, A/B pair and commit SHA, including reverted ones.
  9. **Tightened thresholds**, and the noise measured at the start of Wave D.
  10. **Memory budget table**, socket limit default and hosted profile.
  11. **Correctness:** the deadlock property (Task 5), the existing convergence properties, the iOS matrix, foreground latency and protocol-e2e.
  12. **Carries:** this plan's triage table with a final status per row, and the Phase 6 carries list.
  13. **Not established:** cloud equivalence, physical devices, rendered UI, real APNs, TLS termination cost (the generator speaks to the app directly; production terminates TLS in front), and multi-node.
- [ ] **Step 2: Evidence JSON.** No tokens, DSNs, account or family IDs. It holds:
  - per run: run ID, scenario, load, start and end, verdict, gates, route summaries (p50/p95/p99/max, counts) and resource summaries;
  - SHA-256 hashes of each run's private `result.json` and `audit.json`.
- [ ] **Step 3: Final gates**, on an idle machine, one at a time.

```bash
export GOCACHE=/path/to/bt-gocache-phase5-final
make server-build
make server-test
(cd Server && go vet ./... && go vet -tags identitytest ./...)
(cd Server && go test ./scripts -count=1)
make server-smoke
make server-protocol-e2e
BT_RECOVERY_E2E_MODE=self_hosted make server-protocol-e2e
bash Server/scripts/mcp-e2e.sh
bash Server/scripts/test-performance-runner.sh
bash Server/scripts/test-capacity-runner.sh
pgrep -fl xcodebuild || true
make test
```

Expected: every gate exits 0, and `make test` reports `EXPECTED_TESTS` (update it in the same commit only if a Swift test was added). Record each command's outcome and duration in the report.
- [ ] **Step 4: Commit.**

```bash
/usr/bin/git add docs/verification
/usr/bin/git commit -m "docs(verification): Phase 5 load — PASS at 2x for 30 min on Apple M4 Docker caps, ceiling <x>x, tightened thresholds"
```

Do not merge, push or deploy. Report to the owner.

---

## Phase 6 carries produced by this plan

- **Operator documentation:**
  - the hosted profile (pools, admission, socket limit, GOMEMLIMIT behaviour, PostgreSQL settings) as measured in Tasks 14 and 16;
  - `BT_BODY_READ_TIMEOUT`;
  - the background connection count if Task 17 raised it;
  - the retention windows including D1.
- **Homelab:** `BT_TRUSTED_PROXY_CIDRS` for Tailscale Serve (C37), and the migration number at merge (Task 22's rename procedure).
- **Rollout risks:** global APNs pause on 429 (C39); entitlement UX for lapsed subscriptions (C33); TLS termination cost not measured; the label "Apple M4, Docker CPU caps" (not cloud-equivalent).

## Owner decisions

1. **D1 — audit retention (carried from Phase 3, "ask user then").**
   - At the model, refresh alone appends about 6M account-scope audit rows a day, about 3.1 GB a day at 453 bytes a row (Phase 3 plan :120). Task 13 measures the real figures.
   - Options:
     - (a) keep N months of monthly partitions: tens of GB a month on one node;
     - (b) a short window (for example 30 days) with daily partitions;
     - (c) a spec change: routine refresh rotation writes no audit row; reuse detection, revocation and every other security event keep theirs; the receipt and session rows remain the record.
   - Recommendation: (c), with 13 months for the remaining events. If refresh audit rows must stay, (b).
2. **D2 — OrbStack's VM memory.**
   - OrbStack runs with `memory_mib` 12288 (12 GiB). The declared caps add up to 16 GB (app 8 + db 8), so they cannot both be reached at once.
   - Options:
     - (A) raise OrbStack to 18 GiB (`orb config set memory_mib 18432`). That restarts OrbStack, stopping every container, including other sessions' test databases, and leaves macOS about 6 GiB.
     - (B) keep 12 GiB, run with the declared caps as ceilings, and report the measured peaks with the VM limit stated beside the label.
   - Default if no answer: (B). Task 12's calibration and Task 13's peaks show whether the difference matters.

Not a decision, but needed: the heavy runs need the machine to themselves. Each heavy run blocks other sessions for its whole duration:

| Run | Machine time |
|---|---|
| First measurement (Task 13) | about 1.5 h |
| Each Wave D A/B | 30–45 min |
| Ramp (Task 24) | about 1 h |
| PASS soak with a fresh seed (Task 25) | about 1.5 h |
| iOS matrix (Task 26) | about 1 h |

## Self-review

- **Spec coverage ("Verification → Load"):**

  | Spec item | Where |
  |---|---|
  | Model and 2x | Task 3 `Model10k`/`Scale`, used by Task 10 |
  | SQL seeder at 10,000 families in minutes | Task 4, timed against 15 minutes in Task 13 |
  | Device mix of pull, write, refresh and idle live connection | Task 10, plus snapshot, MCP, push-token and Live Activity shares |
  | CPU, memory and lock profiles per run | Tasks 1 and 2 (pprof cpu/heap/mutex/block, `pg_stat_statements`, `pg_locks`, wait events, Docker stats) |
  | PASS criteria | Task 3 `Evaluate`, Task 11 audit, Task 25 soak |
  | Declared reference node | Task 1, labelled in Task 28 |
  | Measured ceiling | Task 24 |
  | Thresholds tightened | Task 25 |
  | Micro-optimisations gated by profiles | Task 21's eligibility rule |

  **Correctness:**

  | Spec item | Where |
  |---|---|
  | Convergence property | Exists: `internal/sync/convergence_test.go:420,490` |
  | Deadlock property over real commands | Task 5 |
  | iOS suite against the new server | Task 26 (matrix, foreground) and Task 28 gates (two-caregiver, owner reinstall) |
  | Port suites and security invariants | Phases 1–4; kept green by Task 28's `server-test` |

  **The brief's list:**

  | Item | Where |
  |---|---|
  | Compose caps and their verification | Task 1 |
  | Seeder: history, sessions, devices, Live Activity, minutes, deterministic seed | Task 4 |
  | Open loop with coordinated-omission safety | Task 3 |
  | Sockets at 4,000 and 8,000 | Tasks 10 and 14 |
  | Refresh-recoverable | Task 10 |
  | Cursors | Task 10 |
  | Snapshot and MCP shares | Task 10 |
  | SLO, error classes, audit | Tasks 3 and 11 |
  | Profiles | Tasks 1 and 2 |
  | Ramp | Task 24 |
  | Soak | Task 25 |
  | Tuning loop | Task 21 |
  | Storm | Task 15 |
  | Restart | Task 23 |
  | Report | Task 28 |
  | Waves | "Execution" |
  | Constraints | "Global Constraints" |

- **Placeholder scan:** the angle-bracket fields left are values only a measurement can supply: `<n>` in commit messages, `0000NN` before the numbering rule runs, `<date>`, and the root cause in Task 6's commit text. Each has the rule that fills it. No step says "add error handling" or "similar to Task N"; conditional steps carry their decision rules.
- **Type consistency:**
  - `DeviceState`, `FleetHeader`, `WriteFleet`/`ReadFleet` and `CheckFresh` are defined in Task 4 and consumed by Task 10.
  - `LedgerEntry`, `CreateLedger`/`ReadLedger`, `AuditResult`, `RunSummary`, `Evaluate`, `Thresholds`, `SpecThresholds`/`LoadThresholds` and the route constants are defined in Task 3 and consumed by Tasks 10, 11 and 25.
  - `Totals`, `Stats` and `Len` are defined in Task 2 and consumed by Tasks 6, 18 and 23.
  - `AdaptiveBatch.GrowAfter`: Task 17. `applyMemoryLimit`: Task 14. `startProfiling`: Task 2.
- **Review Focus:** every line has a test in its owning task (Tasks 1, 3, 4, 10, 11), named in the Review Focus section.
