# Single-node Sync Capacity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Establish repeatable capacity measurements and improve sustainable sync throughput within explicit resource budgets while retaining correctness and authorization guarantees.

**Architecture:** Separate fixture construction from load generation. Measure the real server composition, introduce explicit database/admission budgets, then implement only optimizations supported by profiles. Keep the existing filesystem authority and Go/PostgreSQL deployment.

**Tech Stack:** Go 1.26.2, pgx v5, PostgreSQL 17, Docker Compose, existing Python supervisor and Swift peer probes.

**Spec:** [Approved design](../specs/2026-10-03-single-node-capacity-mcp-design.md), Track A. Track B follows in [the MCP plan](2026-10-03-private-readonly-mcp.md).

## Global Constraints

- Planning base `add33b7d16e05ab835a297de33032c0bfa0b3099`; its server code matches the audited `1fe8ac4`. Reconcile HEAD, tracker and concurrent changes before execution. Use an isolated worktree at implementation time; preserve unrelated edits and `work/`.
- Existing history validation remains O(committed history bytes) per outer lease. No unchecked TTL readiness cache, delayed corruption detection, weakened revocation, quota bypass or fabricated authority fixtures.
- Test budgets: four logical CPU equivalents/4 GiB combined, and two CPU/2 GiB combined. These are laboratory ceilings, not production changes.
- Warm-up 30 seconds; steady measurement at least 60 seconds; at least 200 successful observations for published latency percentiles; selected envelope soak 15 minutes.
- Healthy connected peer durable-apply p95 <1 second; observable apply reported separately. Unexpected errors <0.1%; zero in-quota admission refusals; zero lost/duplicate acknowledged effects or cross-family data.
- No production load or configuration changes in this plan's authorized local execution. A later Unraid experiment needs its own concrete resource/duration/stop/cleanup approval.
- Every shell command uses `rtk`. All iOS builds/tests use `platform=iOS Simulator,id=<simulator-udid>`; do not create a simulator.
- Commands below run from repository root unless stated otherwise. New commands/flags described below are deliverables, not commands assumed to exist today.

## Review Focus

1. Setup fails halfway, or a process is killed: preserve typed incomplete results and clean only owned resources (Tasks 1–2).
2. A family count implies a larger natural authority head than requested: label incompatible cells, never roll back history (Task 1).
3. Configured database URLs carry conflicting pool settings, or maintenance shares a pool: validate effective ceilings without double counting (Task 4).
4. Slow request bodies and disconnected response consumers: no security fence is held over network waits, and admission tokens are released (Tasks 5–6).
5. Authority history changes/corrupts during optimized requests: warmed caches retain fail-closed behavior and exact revocation ordering (Task 6).

## File and interface map

Create `Server/internal/capacity/` for fixture manifests, workload scheduling and typed results; `Server/cmd/sync-capacity-client/` for an identitytest-only load client; and `Server/scripts/sync-capacity.sh` for a separate runner. Reuse `Server/scripts/lib/disposable-sync.sh` and safe supervisor primitives from `performance_control.py`. Keep the historical `sync-performance.sh` behavior and evidence unchanged.

Extend `Server/internal/diagnostics/` for bounded metrics, `Server/internal/database/pool.go` and `internal/config/` for effective pool budgets, and add `Server/internal/admission/` for shared request admission. Keep measurement and product code separate. Place results under `docs/verification/2026-10-03-single-node-capacity.md` with source-specific raw artifacts in private temporary directories.

## Task 1: Valid reusable synthetic fixture construction

**Files:** Create `Server/internal/capacity/fixture.go`, `fixture_test.go`, `fixture_identitytest.go`, `fixture_identitytest_test.go`; create `Server/cmd/sync-capacity-client/main_identitytest.go` and `main.go` (untagged refusal); modify `Server/cmd/baby-tracker-server/performance_composition_identitytest_test.go` only to expose/reuse test composition where necessary.

**Interfaces:** `FixtureSpec{Families int; DevicesPerFamily int; AuthorityEvents int; CareEntriesPerBaby int}`; `FixtureManifest{Version int; SourceSHA string; InputSHA256 string; Families int; Devices int; ActualAuthorityEvents uint64; CreatedAt time.Time; ExpiresAt time.Time}`; identitytest-only `BuildFixture(ctx context.Context, spec FixtureSpec, destination string) (FixtureManifest, error)`. Private credentials are stored separately from the report manifest.

- [ ] Write `TestFixtureManifestRejectsMismatch` and `TestFixtureRefusesNonDisposableTarget`: assert unknown version, wrong input digest, expired credentials, symlink/existing destination and non-owned database paths are rejected. `TestFixtureNaturalHeadExceedsRequest` must record the actual larger head without rewriting history.
- [ ] Run `rtk proxy env BT_TEST_TAGS=identitytest BT_TEST_PACKAGES='./internal/capacity ./cmd/baby-tracker-server' bash Server/scripts/test-db.sh`; confirm the new tests fail for missing behavior, not environment failure.
- [ ] Implement offline construction through real identity/auth/family/security command modules with production store adapters and an identitytest issuer. Setup may call these modules directly without the HTTP provisioning limiter; measured HTTP composition retains all production quotas. No direct security-table inserts, fake ready gates or shortened persisted deadlines. Seal a consistent stopped database + ledger/head + mirror/identity fixture with private credentials. Restore into owned disposable resources and verify normal application readiness, projection, epoch, counts and access before returning success.
- [ ] Run the same tests green; test untagged command refuses fixture creation with exit 2. Confirm fixture secrets never enter JSON summaries or stdout. Commit scoped files: `test(capacity): build validated reusable synthetic fixtures`.

## Task 2: A bounded load runner that actually measures

**Files:** Create `Server/internal/capacity/workload.go`, `result.go`, `workload_test.go`, `result_test.go`; extend the Task 1 command; create `Server/scripts/sync-capacity.sh`, `test-capacity-runner.sh`, `Server/deploy/capacity/compose.yaml`; reuse existing supervisor primitives without changing prior reports.

**Interfaces:** `Workload{Devices int; ActiveWriters int; EmptyPullInterval time.Duration; WritesPerWriterPerMinute int; BacklogChanges int; Warmup time.Duration; Measure time.Duration}`; `Run(ctx context.Context, fixture FixtureManifest, workload Workload) (CellResult, error)`; `CellResult` records requested/actual load, setup/measurement durations, attempts/successes/refusals/unexpected errors, acknowledged/peer-applied counts, latency samples, resource availability and typed completion reason. Percentiles are nullable.

- [ ] Write `TestCellRequiresMeasurementAndSamples` with assertions `measurement_seconds == 0 => percentiles == nil`, `successes < 200 => percentiles == nil`, nonzero error/refusal counts retained, and a success-only reporter cannot mark an incomplete cell passing. Add killed-child, setup-429, malformed/torn result, cleanup timeout and ownership/symlink rejection controls.
- [ ] Run `rtk proxy go test ./internal/capacity` from `Server` and `rtk proxy bash Server/scripts/test-capacity-runner.sh`; capture the intended red failures.
- [ ] Implement command phases `fixture`, `run`, `report`. Runner CLI: `--profile small|standard --scenario pilot|sweep|soak --source <absolute-clean-checkout> --fixture <private-fixture-dir> --output <fresh-private-dir> --total-seconds <positive-int>`. Reject ambient production connection/path variables and remote Docker contexts. Match baseline/candidate fixture input hashes, source/build identity and resource limits. Keep every cell's checkpoint even on failure. Use monotonic deadlines and private supervisor control, not parsed stdout.
- [ ] Encode standard profile app 1 CPU/512 MiB, database 2.5 CPU/3 GiB, proxy 0.5 CPU/512 MiB; small profile halves each allocation. Refuse unsupported quota enforcement. Report load-generator resources separately. Use 32/128/512 connected devices, two per family; empty pulls every 300 seconds; write activity 1, 15 or 60/minute/active writer as separately labeled offered-load cells. The 32-device pilot uses 16 writers at 15 writes/minute, yielding 240 scheduled writes in 60 seconds without approaching the current 120-pulls/device/minute quota; include peer pulls and setup/warm-up consumption in quota accounting. Low-activity cells may remain sample-incomplete; never substitute their requested count for observations. Add independent contention, 100-change catch-up, slow-consumer and reconnect scenarios. Reject schedules whose current configured device/account/family quotas cannot admit the requested load; overload experiments deliberately exceeding quotas are labeled separately. Vary authority history independently where mathematically feasible.
- [ ] Run tests green and a pilot on disposable local resources with a 300-second total deadline, 30-second warm-up and 60-second measurement. One successful authenticated mutation/peer-pull pair must be observed before expansion. Confirm release product binaries have no fixture hooks. Commit: `test(capacity): add bounded steady-load runner`.

## Task 3: Low-overhead measurements and unchanged-source baseline

**Files:** Create `Server/internal/diagnostics/capacity.go`, `capacity_test.go`, `Server/cmd/baby-tracker-server/resource_sampler.go`, `resource_sampler_test.go`; modify `diagnostics/sync.go`, `security/authority_cache.go`, `security/localstore/integrity.go`, `cmd/baby-tracker-server/main.go`, and Task 2 result aggregation. Record `docs/verification/2026-10-03-single-node-capacity.md`.

**Interfaces:** `CapacityCollector.Observe(phase Phase, elapsed time.Duration)`, `AddIntegrityBytes(n uint64)`, `Snapshot() CapacitySnapshot`; fixed histogram boundaries 0.1/0.5/1/5/10/50/100/500 ms and 1/5/30 s. Fixed pool-role and admission-class enums; no account/family/token labels. `RunResourceSampler(ctx context.Context, interval time.Duration)` samples every five seconds outside request fences.

- [ ] Write tests that reject dynamic labels, keep memory bounded after one million observations, record cache-mutex wait separately from integrity work, and distinguish unavailable/reset counters from genuine zero. Add a test proving memory/system sampling does not occur per request phase or under the security fence.
- [ ] Run `rtk proxy env BT_TEST_PACKAGES='./internal/diagnostics ./internal/security ./internal/security/localstore ./cmd/baby-tracker-server' bash Server/scripts/test-db.sh`; establish red, implement fixed-size histograms/counters and all-pool sampling, rerun green.
- [ ] Run Task 2 pilot with metrics off/on, retaining both overhead measurements. Capture unchanged-source 32-device cells first; expand to 128/512 only when setup, sample accounting and cleanup pass. Benchmark timing uses normal builds; race testing is separate. Actual Swift durable/observable peers use existing `ForegroundLatencyE2ETests.swift`; transport-only clients may not claim rendered or client-store acceptance.
- [ ] Preserve matching baseline product inputs when adding instrumentation; either use identical observational instrumentation in both builds with a recorded patch hash or report baseline latency without internal phase comparisons. Write measured/incomplete conclusions and profile evidence. Commit: `feat(diagnostics): measure sync capacity with bounded overhead`.

## Task 4: Explicit database connection budgets

**Files:** Create `Server/internal/config/database_pools.go`, `database_pools_test.go`, `Server/internal/database/budget.go`, `budget_test.go`; modify `config/config.go`, `database/pool.go`, `cmd/baby-tracker-server/main.go`, `realtime/postgres.go`, `Server/README.md`.

**Interfaces:** `config.DatabasePools{Runtime, Projector, Maintenance, MCP, ServerMax, Reserved int32}`; `LoadDatabasePools(getenv func(string) string) (DatabasePools, error)`; `database.OpenWithMax(ctx context.Context, url string, max int32) (*pgxpool.Pool, error)`; `NamedPoolBudget{Identity string; Max int32}` and `ValidateBudget(pools []NamedPoolBudget, serverMax, reserved int32) error`. Identity is a composition-owned pool identifier, never a logged connection URL. Only actual unique pools count. MCP is disabled/zero in this track.

- [ ] Write `TestEffectivePoolBudget`: standard laboratory values runtime 16/projector 4/maintenance 2/server max 64/reserved 8 pass; duplicate shared maintenance counts once; total above 56, an enabled pool's max <=0, conflicting DSN max and server-reported ceiling below configuration fail. Disabled MCP has zero and opens no pool. Distinct export/cleanup pools each count even if they share a role. Listener reservation is included in runtime occupancy, not counted as an extra connection.
- [ ] Run database/config/main tests red with `BT_TEST_PACKAGES='./internal/config ./internal/database ./internal/realtime ./cmd/baby-tracker-server'` through `Server/scripts/test-db.sh`.
- [ ] Implement `BT_DB_RUNTIME_MAX_CONNS`, `BT_DB_PROJECTOR_MAX_CONNS`, `BT_DB_MAINTENANCE_MAX_CONNS`, `BT_DB_MCP_MAX_CONNS`, `BT_DB_SERVER_MAX_CONNECTIONS`, `BT_DB_RESERVED_CONNECTIONS`. Preserve current defaults when the explicit profile is absent; log only effective numeric budgets. Configure a complete explicit profile atomically; fail on partial/ambiguous configuration. Check PostgreSQL's actual `max_connections` on readiness. Keep role separation and minimum listener/worker availability tests.
- [ ] Rerun green, then compare matched cells with the explicit profile. Do not raise ceilings merely to hide wait time. Commit: `feat(database): enforce explicit connection budgets`.

## Task 5: Shared bounded admission with sync reservations

**Files:** Create `Server/internal/admission/controller.go`, `controller_test.go`; create `Server/internal/httpapi/admission.go`, `admission_test.go`; modify `httpapi/router.go`, `config/config.go`, server composition and Task 3 telemetry.

**Interfaces:** `type Class uint8` with `Sync` and `MCP`; `New(total, mcpMax int) (*Controller, error)`; `TryAcquire(class Class) (release func(), ok bool)`. Release is idempotent. No waiting queue; admission tokens cover finite requests, not upgraded websocket lifetimes. Track B consumes the same controller.

- [ ] Write `TestMCPFloodPreservesSyncSlots`: total 32/MCP max 4 allows four MCP and still admits sync, refuses a fifth MCP, releases on cancellation/panic and never exceeds the total. Add slow-body timeout, long-lived socket upgrade and disabled-MCP tests.
- [ ] Run `rtk proxy go test -race ./internal/admission ./internal/httpapi` from `Server`; confirm red before implementation.
- [ ] Implement cheap global admission before expensive readiness/authentication work with existing body/header/read timeouts. Readiness/liveness retain their own bounded behavior; do not let a saturated workload make liveness fail. Return a typed 503 with retry guidance for global saturation, leaving quota 429 semantics intact. Enforce separate realtime lifetime limits. Add explicit bounded configuration and fixed-cardinality metrics.
- [ ] Rerun green and overload/recovery scenarios. Verify an MCP pool ceiling remains required in Track B: request admission alone is not a database reservation. Commit: `feat(server): bound request admission and reserve sync capacity`.

## Task 6: Profile-driven hot-path improvement

**Files:** Modify only measured hotspots among `Server/internal/httpapi/sync.go`, `auth.go`, `security/authority_cache.go`, `authority_view.go`, `transaction.go`, `localstore/integrity.go`, `recovery/gate.go`, `cmd/baby-tracker-server/main.go`; update corresponding existing regression tests and capacity evidence.

**Interfaces:** Preserve existing external interfaces. Prefer existing `Service.WithAuthorityView(ctx context.Context, run func(AuthorityView) error) error` / `WithAuthoritySnapshot(ctx context.Context, run func(AuthoritySnapshot) error) error` and active fence-lease reuse. Any new cross-module lease interface must be documented and reviewed before callers change. This is an evidence-gated task, not permission for speculative refactoring.

- [ ] Use Task 3 profiles to select one bounded improvement and record trigger, affected files, invariant and expected measured effect in the tracker. If no hotspot is established, stop this task with that finding; complete the other track and propose the next experiment. If the only viable improvement changes integrity/storage trust, return for a separate design decision.
- [ ] Add failing regressions for the selected redundant work, plus warmed-cache historical-byte replacement/deletion, rollback, external append, revocation queued behind in-flight work, canceled request and stale callback use. Assertions require the existing denial outcomes and zero unauthorized durable changes, not just fewer function calls.
- [ ] Implement the smallest change. Do not share cached authority across an ended lease, perform network I/O under the fence, weaken `/readyz`, or remove historical reads merely because the head is unchanged. Preserve full readiness schema/role/key checks unless a tested equivalent invariant replaces duplicated work within the same lease.
- [ ] Run affected database/race packages via `test-db.sh`, then matched baseline/candidate cells. Retain a change only when correctness passes and measured useful throughput/latency improves beyond observed run variation without a resource regression; otherwise revert only this task's own change. Commit an evidence-specific Conventional Commit and update results.

## Task 7: Verify, report and prepare the node experiment

**Files:** Update `docs/verification/2026-10-03-single-node-capacity.md`, `Server/README.md`, `TASKS.md`; create `docs/verification/2026-10-03-unraid-capacity-runbook.md`.

**Interfaces:** A capacity receipt names exact source SHA, fixture and harness hashes, complete cell rows, configured limits, actual load, percentiles/counts, resource series, correctness reconciliation, cleanup results and scope (local versus Unraid).

- [ ] Run `rtk make server-test`, `rtk make server-build`, `rtk proxy go vet ./...` from `Server`, `rtk make server-protocol-e2e`, and the bounded runner controls. If Swift probe code changed, run its focused tests and existing foreground proof. A simulator-dependent command must name the shared UDID.
- [ ] Run a bounded sweep in both profiles and the 15-minute local soak for the highest passing observed profile. Retain errors, sample shortages and unavailable metrics; select no envelope if none passes. Inspect final diff and conduct the chosen workflow's whole-branch review; fix consequential findings and rerun affected gates.
- [ ] Write the Unraid proposal with exact image/source, fresh isolated Compose project, database/storage paths, total CPU/RAM/I/O limits, no production volumes or credentials, test windows and cleanup. Health stop conditions: production readiness failure, sustained resource pressure above the agreed envelope, or interrupted cleanup. Actual numerical host-pressure thresholds and target storage mapping must be checked read-only immediately before requesting run approval.
- [ ] Mark local verification complete separately from pending node measurement. Do not claim this i5 node's supported client count from macOS runs. Commit: `docs(capacity): record measured envelope and node experiment`.

## Plan self-review

The fixture/measurement failure from the historical experiment is covered by Tasks 1–2; instrument overhead and missing data by Task 3; budgets and MCP reservations by Tasks 4–5; integrity-sensitive optimization by Task 6; SLOs, soak, source identity and deployment boundaries by Task 7. Socket fanout is already family-indexed, so no redundant rewrite is planned. This plan intentionally does not promise a hotspot fix or a node capacity figure before measurement.
