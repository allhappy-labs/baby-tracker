# Bounded sync capacity diagnostic — no supported envelope established

The two final synthetic diagnostics retained all 27 cells each, but **neither reached measurement**. Each recorded three modeled pacing-budget stops and 24 typed HTTP429 owner-sign-in refusals. There are zero attempted measurement operations, zero successful peer pairs and no request, durable-peer or observable-peer percentiles. No comparative throughput or latency claim is supported.

The identical fixture schedule encountered different provisioning outcomes: the first32-family cell reached18 fully ready families on current and12 on baseline. Actual authority heads ended at147 and105. These are unmatched setup states, not a capacity improvement. The harness uses one source IP; its provisioning refusals do not establish a distributed-client family limit.

## Sources, environment and bounds

Both runs used the same byte-identical current Swift/Go harness and deterministic private fixture-subject schedule, schema25, Darwin arm64 host, normal identitytest no-race server builds, production Phone/SettingsFeature/store composition, nil polling overrides, normal limits and existing maintenance worker. No baseline product overlays were applied. The pinned baseline checkout was clean before and after execution.

- **current**: source `117d602d752fa295ef11b321c8a6c2fc5dcd334b`, binary SHA256 `1cf7ed2c7e73154a934b01ec2f7050cd978ecfc96b156fedb504095a8dcc4920`, image `sha256:ccd677777ef838a6aa7d74cbbab35048bdbeae3ff4a2448b784d9b9a8cd19960`. Artifacts: `/path/to/artifact`.
- **baseline**: source `d62286430734d469124ce3dec631b582c914aee5`, binary SHA256 `43daac93e1f93f7b0d3e902cb5c4a261012193ba63f870ae4c76cd90dde76d15`, image `sha256:9cb4e446948a9240ab37e157592530a5d368a446ca9f8b0fcb1818daf845c610`. Artifacts: `/path/to/artifact`.

Each runner has a1200-second global deadline starting before setup/build. A private0600 control channel arms the supervisor's118-second monotonic cell deadline and ACKs before Swift work begins; matching finish follows cleanup. Swift's cooperative budget is116seconds. Buffered stdout is diagnostic only. Actual setup+measurement+cleanup elapsed is recorded below; measurement time is0 for every cell. Final runner status0 means completed diagnostic reporting, not passed capacity.

A ready family has two actual running foreground WebSocket tasks, distinct stores and the automatic local-write bridge. Connected engines use the unchanged300-second safety poll; disconnected fallback remains20seconds. Actual engine regression proves20→300→20. Initial/redial admission history is distinct from simultaneous running socket count.

## All cells

`P` = `paced_setup_budget_insufficient`, a modeled setup bound after actual ready-family/head observation. `R` = `setup_or_measurement_refused`, one observed typed HTTP429 at owner_sign_in. Ready means all Phone readiness gates completed. Head is the actual global committed authority revision, including preceding cells. Columns are current/baseline. Every row is incomplete; all requested concurrency above families is labeled same-family contention, with no claim that it was achieved.

| Cell | Families | Concurrency | Requested events | Ready C/B | Actual head C/B | Outcome C/B | Full cell seconds C/B |
|---:|---:|---:|---:|---:|---:|:---:|---:|
| 1 | 1 | 1 | 248 | 1/1 | 7/7 | P/P | 1.360325/1.383164 |
| 2 | 1 | 16 | 248 | 1/1 | 14/14 | P/P | 0.921795/1.387511 |
| 3 | 1 | 64 | 248 | 1/1 | 21/21 | P/P | 1.024509/1.753482 |
| 4 | 32 | 1 | 248 | 18/12 | 147/105 | R/R | 33.179958/54.181637 |
| 5 | 32 | 16 | 248 | 0/0 | 147/105 | R/R | 0.327937/0.256586 |
| 6 | 32 | 64 | 248 | 0/0 | 147/105 | R/R | 0.265864/0.272613 |
| 7 | 128 | 1 | 248 | 0/0 | 147/105 | R/R | 0.239486/0.278644 |
| 8 | 128 | 16 | 248 | 0/0 | 147/105 | R/R | 0.222911/0.342657 |
| 9 | 128 | 64 | 248 | 0/0 | 147/105 | R/R | 0.229989/0.328221 |
| 10 | 1 | 1 | 512 | 0/0 | 147/105 | R/R | 0.207455/0.284701 |
| 11 | 1 | 16 | 512 | 0/0 | 147/105 | R/R | 0.210270/0.276646 |
| 12 | 1 | 64 | 512 | 0/0 | 147/105 | R/R | 0.210901/0.307692 |
| 13 | 32 | 1 | 512 | 0/0 | 147/105 | R/R | 0.217591/0.285892 |
| 14 | 32 | 16 | 512 | 0/0 | 147/105 | R/R | 0.253921/0.319705 |
| 15 | 32 | 64 | 512 | 0/0 | 147/105 | R/R | 0.222067/0.262511 |
| 16 | 128 | 1 | 512 | 0/0 | 147/105 | R/R | 0.260342/0.266351 |
| 17 | 128 | 16 | 512 | 0/0 | 147/105 | R/R | 0.248915/0.272911 |
| 18 | 128 | 64 | 512 | 0/0 | 147/105 | R/R | 0.211834/0.257553 |
| 19 | 1 | 1 | 2048 | 0/0 | 147/105 | R/R | 0.228607/0.261686 |
| 20 | 1 | 16 | 2048 | 0/0 | 147/105 | R/R | 0.253984/0.261106 |
| 21 | 1 | 64 | 2048 | 0/0 | 147/105 | R/R | 0.254287/0.257835 |
| 22 | 32 | 1 | 2048 | 0/0 | 147/105 | R/R | 0.242277/0.278487 |
| 23 | 32 | 16 | 2048 | 0/0 | 147/105 | R/R | 0.272907/0.267538 |
| 24 | 32 | 64 | 2048 | 0/0 | 147/105 | R/R | 0.258067/0.266572 |
| 25 | 128 | 1 | 2048 | 0/0 | 147/105 | R/R | 0.243643/0.265671 |
| 26 | 128 | 16 | 2048 | 0/0 | 147/105 | R/R | 0.266917/0.346581 |
| 27 | 128 | 64 | 2048 | 0/0 | 147/105 | R/R | 0.275835/0.282270 |

For cells1–3, observed heads7/14/21 plus the remaining planned rotation upper bounds give27/34/41 events respectively, below248 in both builds. The model assumes at least6seconds per shared-IP rotation, ignores the stricter22seconds/device spacing and request cost, and therefore overestimates attainable transitions. A recoverable rotation creates one session_rotated authority event; its receipt acknowledgment does not create another. This is a bound on this intentional setup schedule, not observed refresh quota exhaustion or measured service capacity. No aging rotations were attempted in these final cells. Normal configured refresh quotas remain IP120/10minutes, account60/device30; live admission remains12/device/minute.

All54 rows have request/durable/observable distributions `null`, no pairs, measurement_seconds0, and active measurement request/write/pending peaks0. Each run has24 observed setup rate-limit failures,0 recorded generic errors and0 recorded timeouts; retries are unobserved/null. The raw `successful_qps:0` field is a count-derived sentinel with no measurement interval and must not be interpreted as measured throughput. The deterministic40/40/20 scheduler,200-success target,30–60second measurement and separate100-change backlog were not exercised live by these runs. There is no aged248/512/2048 capacity or latency result.

## Resource evidence and limitations

The following are final snapshots after failed setup, not per-cell profiles or comparable steady load. Memory is Docker MemUsage, not measured process RSS; NetIO/BlockIO are Docker cumulative counters. CPU is the Docker snapshot percentage, not CPU-seconds.

| Build/component | CPU | Memory | Network in/out | Block in/out | PIDs |
|---|---:|---|---|---|---:|
| current/app | 1.65% | 20.32MiB / 11.72GiB | 89.5MB / 12.9MB | 11.9MB / 4.21MB | 16 |
| current/database | 1.09% | 113.8MiB / 11.72GiB | 11.9MB / 88.6MB | 44.5MB / 116MB | 14 |
| baseline/app | 1.74% | 19.09MiB / 11.72GiB | 67.4MB / 11.3MB | 24.1MB / 3.01MB | 16 |
| baseline/database | 0.02% | 113.6MiB / 11.72GiB | 10.7MB / 66.8MB | 95MB / 110MB | 14 |

Both final pg_stat_activity snapshots counted14 connections. This is not per-pool occupancy, an available-capacity bound or evidence of zero pool wait. database.Open uses pgxpool.New without an explicit MaxConns setting; runtime, projector and cleanup connections follow existing defaults/configuration, and the realtime listener uses the runtime pool. No fixed10-connection claim or pool increase is made.

Per-cell CPU-seconds, allocations, RSS, network/filesystem bytes, pool-acquire wait, transaction/family/security fence waits and integrity-pass durations are unobserved/null. Retained server-phase timings cannot substitute for integrity-pass measurements. O(committed history bytes) validation and all authority/fence checks remain intact. These observations cannot diagnose O(N) as the bottleneck; no optimization or security-contract relaxation is justified by this matrix.

## Reproduction and verification

From the sync-reliability worktree, each exact command ran to status0:

```sh
rtk proxy env BT_SYNC_PERF_PROBE=1 bash Server/scripts/sync-performance.sh --mode matrix --duration 30 --total-seconds 1200 --output /path/to/artifact
rtk proxy env BT_SYNC_PERF_PROBE=1 bash Server/scripts/sync-performance.sh --mode matrix --duration 30 --total-seconds 1200 --baseline-source /path/to/artifact --output /path/to/artifact
```

Fresh focused verification: tagged Go package11 tests; untagged report2 tests; Swift3 harness tests plus1 actual-engine polling test; resource-free supervisor SIGTERM/SIGINT, delayed/partial control, wrong ordinal, stale/missing finish, cleanup-timeout and retained-row controls; actual missing configuration and expired global-budget preflight controls. Untagged client builds and refuses execution with exit2. The unconfigured Swift matrix test returns without live work; only the two opt-in runs prove actual setup.

Each final cleanup artifact confirms containers0, volumes0 and secrets_removed=true. The baseline remains exact clean d622864. No credentials, fixture subjects or family/account IDs occur in retained cell/stage artifacts. Production load, rendered UI, physical devices, app installation, merge, push and deployment were outside scope.

## Historical corrections

Attempts a/b were explicitly aborted during harness refinement and cleaned; they are not performance evidence. Attempt c actually retained27 rows (15 generic failures,12 rate-limit refusals, head105, zero attempts/pairs), contrary to an earlier mistaken report of missing records. The connected poll assertion incorrectly expected20seconds instead of300; deleted raw logs prevent attributing every generic exception to that defect. Attempt d had two unresolved aging errors at heads65/129, three socket-admission timeouts and a cumulative-dial-count invariant failure; it used the inadequate stdout-driven watchdog. It is diagnostic history only. The final E pair uses the corrected running-task gate and direct deadline/ACK channel.

Recommendation: retain these bounded setup findings and do not publish a supported capacity envelope, baseline improvement or foreground latency acceptance from Task9. The earlier Task8 standalone200-pair proof is a separate experiment and is not substituted for this matrix. Further matched aged-fixture measurement requires an explicitly revised experiment within unchanged integrity and quota constraints.


## Final verification checkpoint

Task10 verification finished at `a34fe2f1008608fd5cdcec169296c93c6bd469c5`. The user then requested a quick stop with no more tests; no owned test/build remained running. Task9 scoped re-review has closed the original findings; the last helper corrections received source-only review without tests. Whole-branch review remains outstanding. This document does not declare the whole plan complete or release readiness.

The earlier E matrix is **historical pre-fix, pre-instrumentation evidence**. Its54 incomplete cells, zero measurement attempts/pairs and absent per-cell instrumentation cannot establish acceptance of the revised harness. Later timeout/checkpoint/baseline-directory controls and real metric wiring were verified separately; the matrix was not rerun. No supported capacity envelope, matched-baseline improvement or aged-history latency acceptance is established.

The separate Task8 complete-run raw pairs were independently recomputed: **200 healthy pairs**, durable p95 **0.817504667s**, observable p95 **0.817507459s**, four durable samples at or above one second. Normal timers and a no-race server were used. This is the standalone synthetic experiment's result, not final-source matrix or physical/rendered UI latency.

### Commands and outcomes

Artifacts below are under `/path/to/artifact` unless stated otherwise. The initial product boundary was `f09b4ca`; test repairs are `36831e1`, accessibility repair `69d19d5`, and instrumentation/harness repair `a34fe2f`.

| Check | Outcome and source boundary |
|---|---|
| `rtk make server-test` | **Failed initially**: snapshot deadline returned `service_unavailable`; stale script assertion looked for moved nginx directives in the wrapper. Other individual packages, including auth/security/localstore/recovery/readiness, passed. `server-test.log`. |
| `rtk proxy env BT_TEST_PACKAGES="./internal/diagnostics ./internal/security/localstore ./internal/security ./internal/records ./internal/httpapi ./cmd/baby-tracker-server ./scripts" bash Server/scripts/test-db.sh` | All7 full affected packages passed with real disposable PostgreSQL, `-race -count=1 -p=1`, against frozen instrumentation and reviewed test repairs. `server-affected-race.log`. Later performance-runner-only baseline-cwd controls were covered by Task9's separate RED/GREEN control, not this earlier all-scripts run. |
| `rtk proxy env BT_TEST_PACKAGES="./internal/sync" BT_TEST_RUN="^TestSnapshotDeadlineRollsBackAllDurableRows$" bash Server/scripts/test-db.sh` | Exact repaired regression passed against final instrumentation, preserving deadline and zero durable-row assertions. `snapshot-deadline-race.log`. |
| `rtk make server-build`; `rtk proxy go vet ./...` from Server | Passed Linux amd64/arm64 normal release build and vet. Product input hashes match the instrumentation subsequently committed in a34fe2f. Owned builder removed. |
| `rtk make server-protocol-e2e` | Hosted standard gate passed exit0, actual Docker/TLS/client composition. Runner enforces step18 and cleanup. RTK success compaction omitted trailing proof text; this is disclosed rather than presented as a retained literal marker. `server-protocol-e2e.log`. |
| `rtk proxy env BT_PROTOCOL_DIAGNOSTICS=1 BT_RECOVERY_E2E_MODE=self_hosted bash Server/scripts/protocol-e2e.sh` | Passed actual self-hosted recovery, five Swift crash boundaries, four server fault boundaries and bounded metric smoke. Raw output retained; sanitized exact proof in `selfhosted-safe-proof.json`. |
| `rtk make test` | Final a34fe2f source passed exactly **1191** registered tests. `make-test-final.log`. Unconfigured opt-in tests are not represented as live measurements. |
| `rtk make build 'SIMULATOR=platform=iOS Simulator,id=<simulator-udid>'` | Final a34fe2f incremental app build passed. `make-build-final.log`;445 input hashes in `swift-final-input-hashes.json`. |
| Direct `xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,id=<simulator-udid>' -parallel-testing-enabled NO -collect-test-diagnostics never -only-testing:BabyTrackerUITests/HomeSyncRecoveryUITests` with fresh owned result bundle | Final **2/2 passed** after69d19d5 accessibility fix. Result `/path/to/artifact`, log same base.log; named synthetic screenshots in same base-attachments. Task10 independently inspected final title/action/post-tap screenshots and assertions. |

The failed initial broad server gate plus corrected scoped checks are **not a single full-suite green run**. Race runs establish correctness, not normal-build performance. Three owned race projects and both protocol projects were independently verified to have zero remaining containers/volumes; temporary protocol images were absent. No source edits or commits were made by Task10.

### Actual recovery and metric evidence

The self-hosted image was `sha256:bfcb4138dbb460f42638cd99edf0c51671560b18b355d332c9dd5d445296f318`, binary SHA256 `c6c1d25c7191a794a59d1773f0423b46b54859a8d70705dc34f12e3003e233f3`, schema25, arm64. Its label is69d19d5 plus the frozen instrumentation delta subsequently committed as a34fe2f; it is not falsely labeled a clean69d19d5 build. The final15-file Task9 manifest SHA256 is `af68d34397efe514a9b7af9f0d3a9a4cafd5866fca7dd6eb01774fce825cbebf`. The final baseline-cwd shell correction did not change executing protocol/helper/product inputs.

Actual production-cadence cleanup-worker admission, capability and isolated maintenance privileges were verified. Before-authority-commit, after-authority-commit, after-projection and lost-response server faults recovered one child/receipt. Separate Swift processes at after-decode, before-Keychain, after-Keychain, after-completion and after-ack recovered exact child credentials, exact prepared version when one existed, exactly-once ordinary sync and zero receipts after acknowledgement. Real persisted lease expiry was awaited; no lease rows/timers were shortened. Standard hosted protocol is separate from this self-hosted recovery proof.

The recovery smoke observed241 Docker/DB samples, maximum16 DB connections, and actual scalar totals for security fence acquisition9,119,864ns, family lock acquisition50,140,540ns, transaction acquisition15,210,815ns and historical inventory verification187,234,832ns. These totals are diagnostic aggregates across this recovery workload, not per-request distributions or a capacity cell. Authority/database/observer totals were also present. Inventory-construction/extension timing was not observed in the retained request capture. Process CPU/allocation and runtime-pool counters reset across deliberate restarts, so the reported first-to-last deltas are explicitly null. Maximum observed runtime-pool connection count was4; that is not its configured ceiling or a total pool budget. Current RSS remains unavailable; peak RSS is a distinct fallback. Non-runtime pool costs remain unmeasured, and BeginTx acquisition is not a pure transaction-fence wait. `selfhosted-safe-proof.json` retains the exact typed values and limitations.

### Security coverage inventory

These source assertions identify the regression coverage run by the broad server race gate. They do not claim a Cartesian combination of every attack, endpoint and cache state:

| Boundary | Concrete regression assertions |
|---|---|
| Cold/warm authority drift | `Server/internal/security/authority_cache_test.go`, `TestWarmAuthorityRejectsRollbackAndCorruption`, explicitly loops cold/warm and rollback/hash/generation/missing/bytes/projection/epoch/identity/quarantine; `TestWarmAuthorityAcceptsExternalExtension` also loops both states. |
| Selected historical disk bytes | `Server/internal/security/localstore/integrity_test.go`, `TestIntegrityInventoryRejectsSameHeadChanges` checks corrupt/delete/directory/symlink/oversize/replacement after inventory creation; `TestIntegrityInventoryRejectsOpenBoundarySwap` checks path swaps. Cold `ledger_test.go/TestLedgerFaultMatrix` checks changed bytes, missing middle, truncation, head mismatch and commit ordering. |
| CAS and successor ordering | `authority_successor_test.go/TestAuthoritySuccessorCASLossFailsClosed` covers warmed CAS/projection failure; both successor publication tests assert publication before exclusive-fence release and unchanged prior owner. `localstore/fence_test.go/TestIndependentProcessHeadCAS` races two actual processes. |
| Capability and mutable escape | `authority_view_test.go` checks lease/metadata expiry, detached selections, no ordinary read clone, current nested lease and detached values unable to alter preparation. `authority_successor_test.go/TestStalePreparedCapabilityCannotCommit` and `prepared_test.go` check stale, foreign and retained private capabilities. |
| Concurrent readers and fence ordering | `authority_cache_test.go/TestWarmAuthorityConcurrentExtensionPublishesOneOwner` checks16 simultaneous readers after an external append, not a simultaneous writer. `transaction_test.go/TestGuardTransactionFenceCompletesBeforeRemoval` and `TestDecisionReadLeaseDrainsEnteredGoroutineBeforeTransactionAdvances` check serialization and draining; successor tests cover immutable publication. |
| Readiness, roles and recovery keys | `Server/cmd/baby-tracker-server/readiness_integrity_test.go/TestReadinessWarmCacheStillRejectsDrift` warms real readiness then alters identity/epoch/head/projection/schema/role/outage/keys. `credential_inventory_test.go` covers deadline revalidation, required-key retention, per-lease uncertain/error retry, new-head union, concurrent coalescing and reentry denial. |
| Immediate authority revocation | `transaction_test.go/TestAccountGuardRejectsSameEpochAuthorityChanges` checks identity/device/session revocation; sync `TestRevocationCommittedBeforeSQLAuthorizationDeniesStaleRequest` checks stale requests. `realtime/authorizer_test.go` checks member removal and expired/revoked/wrong-epoch sessions; hub invalidation tests check detachment before potentially stalled closes. |
| Notification isolation and backpressure | `realtime/postgres_test.go/TestNotificationLookupRejectsForeignDomains`, malformed/duplicate/oversized/foreign/epoch decoding controls, `hub_test.go/TestIndexedPublishTargetsOnlyFamily` and `TestFamilyIndexLifecycleAndConcurrentPublish` retain routing isolation. Socket-bound/coalescing/slow-consumer and reservation-bound tests retain bounded backpressure. |
| Cancellation and cleanup | `localstore/fence_test.go/TestFenceHonorsCancellation`, `integrity_test.go/TestIntegrityInventoryCancelsWithinReadAndClosesDescriptor`, transaction lease-draining controls and hub deadline/shutdown tests exercise cancellation and cleanup at their component boundaries. |

### Rendered UI, bundle and preservation evidence

Read-only preflight confirmed the existing shared iPhone17Pro `<simulator-udid>` on iOS26.5, compatible with the iOS26.0 target. Every iOS gate explicitly selected it; no devices/runtimes were created, cloned, reset or uninstalled. Broad `make test-ui` was excluded because it deletes persistent shared App Group data; only the isolated in-memory Home fixture ran.

The initial actual UI run failed Ukrainian accessibilityXXXL hittability. Its screenshots showed fixed care controls covering the sign-in action. A separate worker repaired accessibility layout and strengthened targeted scrolling/idle-state assertions in69d19d5. Final EN/UK screenshots show a fully reachable sign-in action, full text wrapping, and retained queued-entry explanation after tapping; both UI tests passed. The initial Xcode automatic diagnostic client stalled after assertions and was terminated only after controller authorization and fresh exact-child/parent/UID verification. No shared simulator service was terminated. Broad automatic diagnostics are not published; final rerun disabled them while retaining explicit screenshots.

Final actual app-bundle inspection found SwissPEZZ CSV/attribution/manifest, with CSV/attribution hashes matching the manifest, and no ZIP/PDF/LeaseProbe/performance harness artifacts. `app-bundle-final.json` records app executable SHA256 `9b41a0f0b176bd78d9acc56a8c6fb4c29b8858117b9c7d52ce7e3658d5afbf6c`. Main retained only its pre-existing dirty TASKS.md and untracked work/; Growth sibling remained clean; no ZIP/PDF is tracked.

### Remaining limits and decisions

The global exclusive writer and **O(committed history bytes) per outer lease** remain intentional constraints. Source `database.Open` uses `pgxpool.New` without an explicit fixed max/min. Runtime/projector pools are distinct; cleanup has a separate pool or reuses configured export maintenance; LISTEN acquires a runtime-pool connection. Socket defaults remain1/device,3/account,1000/process. Neither the historical14-connection snapshot nor recovery-smoke16-connection maximum establishes the configured total ceiling, safe pool size or supported envelope. No blind pool increase is justified.

Physical-device local-save, peer durable/observable and visible-UI timings, installed-client source identity and production behavior remain unverified. Background execution/APNs remains opportunistic; direct engine invocation is not OS delivery. Deployment requires separately reviewed SHA/immutable image, backups, schema22-to-reviewed-source migration/compatibility and rollback constraints; after authorization, verify deployed identity/schema/readiness/socket and bounded aggregate health. No phone installation, merge, push or deployment was performed.

The recovery one-retry sign-in-action delay and duplicate unbound `ExecutePrepared` epoch adapter remain parked minor items. Final helper runtime validation and whole-branch review remain outstanding after the sealed source-only review; no further tests are authorized in this checkpoint. No user input is needed to preserve these results; capacity experiment redesign and rollout are separate decisions.


## Seal instruction and final reporting boundary

The user requested quick sealing with no further tests. All gates listed above completed beforehand. Task9 scoped re-review subsequently confirmed I1–I4 and M1 addressed and found two new reporting-only bugs: malformed-row attribution could invalidate the previous cell, and Docker network/filesystem delta aggregation did not detect an interior reset. The final narrow helper corrections will be committed without executing tests; the a34fe2f gate evidence applies to unchanged product code, not validation of those last helper corrections. Historical network/filesystem delta values in the recovery smoke are not accepted as validated reset-free measurements. Whole-branch review remains deferred. The sealed handoff records the final source/review boundary.


Final helper-only commit: `f5e83c7` — malformed-row own-cell/global-failure attribution and adjacent Docker counter/timestamp ordering corrections. HelperSHA256 `c7b268b8781fad9c80f695da60c44b82df52bf1547b59fc15d595085411252d3`. No tests, synthetic executions, builds or live gates were run for these last corrections. Product/Swift/instrumentation inputs remain unchanged from the verified a34fe2f checkpoint; final helper validation and whole-branch review remain deferred. A narrowly scoped read-only finding-closure review is recorded in the retained task workspace.

Final scoped review: N1/N2 closed by independent source inspection of `a34fe2f..f5e83c7`, with no apparent new Critical/Important breakage. The final helper remains UNTESTED. This is not whole-branch approval. Reports/ledgers and all execution rulings are retained; no worktree or evidence was deleted.


## Bounded validation resumed — 2026-10-01

The user authorized quick bounded tests, superseding the no-test instruction for this focused scope. The resource-free `rtk proxy bash Server/scripts/test-performance-runner.sh --revision-controls-only` ran with a180-second whole-command limit. The first run failed in.142s: invalid resource timestamps were filtered before window validation. Commit `251ba7f` narrowly corrects that and adds persistent N1/N2 regressions. One rerun passed exit0 in1.624s, covering malformed cross-cell attribution/global failure, counter resets/order/nonfinite timestamps/real zero deltas in both aggregators, killed-child checkpoint preservation and bounded cleanup/recovery pointers. Final helperSHA256 `63ce00e2c193e41e18745462c09a20ac025ff3b4988777148d8f5ec4a41cb649`; testscriptSHA256 `8cceacff860d5f10428a10c170c082a579b74e3e860f401fb3ce9767add0de13`. Captured log/result retained under `/path/to/artifact`; ignored task report contains full output/source boundaries. Owned children exited/reaped and scratch enumeration was zero. Controller read the complete output, result and scoped source diff and checked source hashes.

Final reporting-helper corrections now have focused passing regression evidence. No broad tests/builds/live server/capacity run was repeated. Whole-branch review, capacity acceptance, physical-device/provider/production proof and parked minor items remain outstanding; no merge/push/deployment/app update. Historical network/filesystem deltas remain unvalidated; these tests do not retroactively establish their series integrity.
