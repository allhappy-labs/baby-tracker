# Sync Performance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve fresh fail-closed authority verification while reducing reconstruction cost and measuring foreground local-save to peer durable/observable application p95 below one second.

**Architecture:** One immutable authority owner reuses validated decoding only after historical bytes and persistent authority bindings are freshly checked within existing fences. Detached selectors replace whole-state read clones, and a family index routes live hints. An opt-in disposable full-composition harness measures the actual Swift peer path separately from HTTP request costs.

**Tech Stack:** Go, PostgreSQL/pgx, filesystem authority ledger, Swift Testing/SwiftData/Observation, Docker Compose and TLS nginx proxy.

**Spec:** `docs/superpowers/specs/2026-10-01-sync-performance-design.md`; read the approved companion `2026-10-01-session-recovery-design.md` and its implementation plan before receipt/inventory changes.

## Global Constraints

- Written plan approved by user; subagent-driven execution selected. No merge, push, app installation or deployment is authorized here.
- Foreground healthy, caught-up, valid-session, established-socket target: **foreground local save to peer durable application p95 below 1 second**; also measure observable publication and physical visible UI separately.
- Historical integrity remains **O(committed history bytes) per outer lease**. No TTL, mtime/watcher proof, head-only proof, deferred historical audit, or silent known-rollback rebuild.
- Shared/exclusive process and filesystem fences remain. Never extend a shared lease across a whole HTTP request, response/radio lifetime, or mutation batch.
- Fresh recovery identity/epoch, trusted head, projection and quarantine checks remain. Cold start, coordinated recovery and explicit deep audit retain full strict chain/event validation.
- No full-state clone on ordinary read/auth/sync paths; at most one clone per committed authority write initially. Escaping selected maps/slices must be detached.
- Prepared capabilities remain private, principal/scope/action/head/epoch bound; expired lease borrowing and late goroutines fail closed. Publish successor/invalidation before releasing exclusive fence.
- Retain protocol/schema/security suites, default quotas, slow-consumer isolation and listener connection budget. No new persistent-map dependency, blind pool/limit increase or horizontal-writer claim.
- Matrix families **1, 32, 128**, concurrency **1, 16, 64**, committed security events at least **248, 512, 2,048**; report actual sizes, contention and refusals.
- Warm-up then bounded **30–60 second** measurement, at least **200 successful observations where attainable**, **2 minutes per cell**, **20 minutes total**. Incomplete cells/timeouts are results.
- Separate normal-build timing from race correctness. Synthetic source results do not establish installed-phone or production behavior. No production load.
- Diagnostics expose bounded codes/times and ephemeral IDs only; never credentials, Apple proofs, account/family identifiers or care payloads.

## Review Focus

- Same-head selected-file substitution after warming must deny the next outer lease (Task 2).
- A retained callback view or nested goroutine after lease closure must not grant authority (Task 3).
- Receipt retirement racing readiness must not permit removal of a still-required key (Task 5).
- Family socket release racing Publish must preserve index consistency and avoid cross-family writes (Task 6).
- A signed-out writer or disabled hint path must not pass a foreground latency claim via fallback polling (Tasks 1 and 8).

## Source map and runners

Work only in `.worktrees/sync-reliability`, branch `fix/sync-reliability`; inspected source `1b44888`. Preserve current dirty specs/verification docs. Paths are case-sensitive (`Server`, `Packages/BabyTracker`), and Go internal-import rules require Go harness code under `Server`. Swift store-dependent code/tests stay in existing package targets; new test targets require `Package.swift` changes.

Existing production authority: `Server/internal/security/coordinator.go`, `prepared.go`, `transaction.go`, `state.go`, `localstore/ledger.go`; hosted adapter remains full-validation fallback. Realtime: `hub.go`, `postgres.go`, `authorizer.go`. Real application wiring/readiness: `Server/cmd/baby-tracker-server/main.go`. Sync commit observer is in `Server/internal/sync/{mutation.go,integration_test.go}` and `Server/internal/delivery/observer.go`.

Existing commands below were verified by reading the runner definitions, not executing product tests during planning:

- From repository root: `rtk proxy env BT_TEST_PACKAGES='./internal/security/... ./internal/recovery/...' BT_TEST_RUN='TestName' bash Server/scripts/test-db.sh`. Runner owns disposable PostgreSQL and always invokes `go test -p=1 -race -count=1`; tags use `BT_TEST_TAGS=identitytest`. Never supply an external DSN.
- From `Server`: `rtk go test -count=1 ./internal/security/localstore -run TestName` for filesystem-only cases; use database runner for DB-backed packages.
- From `Packages/BabyTracker`: `rtk swift test --filter FamilySyncEngineTests` for targeted iteration. Final package gate is root `rtk make test`; reconcile the current `Makefile` literal `EXPECTED_TESTS` after completed recovery tasks, then update by the actual newly registered tests only (1112 was the planning baseline).
- Root `rtk make server-test`, `rtk make server-build`, `rtk make server-protocol-e2e`, `rtk make build`, `rtk make test-ui`. Existing protocol runner owns disposable app/DB/TLS resources, selects hosted mode, runs race Go gates and filtered Swift tests, and checks a completion proof line. A skipped/early-return Swift E2E test is insufficient.
- Every commit stages only the task's reviewed paths and uses `rtk git commit -m 'perf: ...'` / `feat:` / `test:` as appropriate. Commit actions are local only, after tests/review and when execution authorizes commits.

---

### Task 1: Instrument and reproduce the live chain without changing timers

**Files:** Modify `Makefile`; create `Packages/BabyTracker/Sources/BabySync/SyncDiagnostics.swift`, `Packages/BabyTracker/Tests/BabySyncTests/SyncDiagnosticsTests.swift`; modify `FamilySyncEngine.swift`, `SyncCoordinator.swift` in that source directory, `Packages/BabyTracker/Sources/SettingsFeature/FamilySyncSettingsModel.swift`, `FamilySyncComposition.swift`, `Packages/BabyTracker/Sources/BabyStore/BabyStore.swift`, `BabyStore+RemoteApply.swift`, `AppGroupWriteBoundary.swift`, `TimerIntentReducer.swift`, `BabyStore+Outbox.swift` in that store source directory, and `Packages/BabyTracker/Tests/SettingsFeatureTests/FamilySyncWiringTests.swift`; create `Server/internal/diagnostics/sync.go`, `sync_test.go`; modify `Server/internal/httpapi/{router.go,sync.go}`, `Server/internal/security/{coordinator.go,transaction.go,transaction_test.go}`, `Server/internal/sync/mutation.go`, `Server/internal/delivery/observer.go`, `Server/cmd/baby-tracker-server/main.go` to inject opt-in timing at request/authority/transaction/observer boundaries.
**Interfaces:** Produce `public struct SyncDiagnosticEvent: Sendable` with `phase: SyncPhase`, `elapsedNanoseconds: UInt64`, `status: SyncDiagnosticStatus`, `correlation: UUID`; `SyncPhase` cases are `localCommit`, `enqueue`, `requestStart`, `requestComplete`, `hintReceived`, `peerPull`, `remoteCommit`, `observablePublish`, and `SyncDiagnosticStatus` cases are `success`, `retry`, `refused`, `inactive`. BabyStore produces `StoreSyncMilestone { phase: Phase; monotonicNanoseconds: UInt64 }` with Phase `localCommit`, `remoteCommit`, `observablePublish`, plus optional `@MainActor (StoreSyncMilestone) -> Void` callback; BabySync maps it to its sink without a BabyStore→BabySync dependency. Production events never include persistent entity IDs; E2E correlation stays in an ephemeral in-process map and is excluded from artifacts; `public protocol SyncDiagnosticSink: Sendable { func record(_ event: SyncDiagnosticEvent) }`. Server counterpart: `type Phase string`, `type Recorder func(context.Context, Phase, time.Duration)` in diagnostics, with fixed phase allowlist and no arbitrary fields; optional options Recorder defaults nil. Add detached observable engine status with active/sign-in/live/phase states, wired to the existing settings model.
- [x] Write `diagnosticsCoverSaveThroughObservableApply`, `signedOutCompositionReportsInactive`, `diagnosticsContainNoCareOrCredentialValues`: assert
  ordered save/enqueue/start/HTTP/peer apply/publication milestones, writer commit starts clock, inactive engine cannot report healthy live sync, and
  sentinel secrets/IDs/payloads never appear in recorded events.
- [x] Run `rtk swift test --filter SyncDiagnosticsTests` and `rtk swift test --filter FamilySyncWiringTests` from package; expect failure on missing
  sink/status milestones.
- [x] Implement opt-in phases around existing durable save/apply boundaries; server timing codes are `authority`, `database`, `observer`; they cover those phases without arbitrary payload fields. Keep 300 ms
  debounce, 20 s fallback and 300 s socket safety polls unchanged. HTTP response completion is its own milestone.
- [x] Run root DB runner `BT_TEST_PACKAGES='./internal/diagnostics ./internal/httpapi ./internal/sync ./internal/delivery'` for redacted timing and observer regressions. Rerun targeted Swift tests, capture a deterministic established/dialing/reconnecting/refused/rate-limited/capability-mismatch/retry-queued/sign-out
  trace with existing engine/socket test fakes; retain only redacted artifacts.
- [x] Review and commit `feat: add bounded sync path diagnostics`; bump exact package test total with tests.

After Task 1 is reviewed, record that HEAD as the unchanged optimization baseline: it includes completed recovery work and instrumentation, but no cache/index changes. Benchmark both server builds with the same current Swift measurement clients, pinned fixture manifest, protocol/schema and instrumentation. Build harness-only helpers from the current tree against each server; never overlay changed product code into the baseline.

### Task 2: Select and verify canonical committed-file inventories

**Files:** Create `Server/internal/security/integrity.go`, `Server/internal/security/localstore/integrity.go`, `Server/internal/security/localstore/integrity_test.go`; modify `Server/internal/security/localstore/ledger.go`, `Server/go.mod`, `Server/go.sum`.
**Interfaces:** In `security`, define `CommittedFile { Revision uint64; SelectedName string; Prior Head; EventHash, RawDigest [32]byte }` and `CommittedInventory { Head Head; Files []CommittedFile }`. Optional `IntegrityLedger` exposes `ReadValidatedInventory(ctx context.Context, head Head) (CommittedInventory, []Candidate, error)`, `VerifyInventory(ctx context.Context, inventory CommittedInventory) error`, and `ReadValidatedExtension(ctx context.Context, inventory CommittedInventory, next Head) (CommittedInventory, []Candidate, error)`. Returned candidates contain detached bytes. No path supplied by callers may escape the generation directory.
- [x] Write `TestIntegrityInventoryRejectsSameHeadChanges`: warm, then corrupt/delete/nonregular/symlink/oversize/replace canonical selected bytes;
  assert errors before a visitor/authorization can run, including unchanged head. Assert uncommitted siblings remain excluded.
- [x] Write `TestIntegrityInventoryExtensionAndCancellation`: external valid append yields exact selected prefix plus extension; altered
  prefix/invalid link fails; pre-cancelled and mid-read cancellation release files and create no walk scratch artifacts. Verify same-head reads every
  selected file once and performs no parsing, even if file stats are unchanged.
- [x] Run `rtk go test -count=1 ./internal/security/localstore -run TestIntegrityInventory` from `Server`; expect missing API failures.
- [x] Implement bounded `68<<10` canonical disk-envelope reads with the existing regular-file/symlink contract; cold selection strictly
  decodes/hash/links, stores exact filename/raw SHA-256, and reuses retained selected order. Extensions verify prefix bytes then strictly select new
  suffix backward and apply forward. Preserve revision bounds and genesis validation.
- [x] Run the same tests and full `rtk go test -race -count=1 ./internal/security/localstore`; review contract equivalence with existing
  `WalkCommitted` cases, then commit `perf: verify committed file inventories without repeated decoding`.

### Task 3: Immutable authority owner and fenced read-only capabilities

**Files:** Create `Server/internal/security/authority_cache.go`, `authority_view.go`, `authority_cache_test.go`, `authority_view_test.go`; modify `coordinator.go`, `prepared.go`, `operator_prepared.go`, `transaction.go`, `state.go` and affected existing snapshot tests in the same directory; modify `Server/cmd/baby-tracker-server/main.go` binding composition.
**Interfaces:** Private `validatedAuthority` owns state/head/inventory/epoch binding; `Service.loadValidatedAuthority(ctx context.Context) (*validatedAuthority, error)` runs only under an existing fence. `AuthoritySnapshot` keeps Head/RecoveryEpoch and private token and adds `View AuthorityView`; retain its detached legacy `State` only until Task 4 migrates all consumers so this task compiles independently. New `WithAuthorityView` avoids that legacy clone. Add `AuthorityBinding { InstanceID string; Epoch uint64; TrustedHead Head }` and optional `Options.Binding func(context.Context) (AuthorityBinding,error)`, populated from existing `recovery.Store.LoadUnderFence`; preserve Epoch fallback for existing test/hosted adapters with no cache acceleration until an identity binding exists. `AuthorityView` exposes existing selector names/signatures (`Account`, `Device`, `Session`, `Family`, `FamilySlot`, `Membership`, `Memberships`, `Invitation`, `Entitlement`, `RecoveryClaim`, `Receipt`, `ResolveIdentityAuthority`, `ReferencedCredentialKeyIDs`) over private state; value/bool selectors return zero,false when expired, error selectors return `ErrUnavailable`. Add `Service.WithAuthorityView(ctx context.Context, run func(AuthorityView) error) error`. Tokens carry private lease validity; no writable state escapes.
- [x] Write `TestWarmAuthorityRejectsRollbackAndCorruption`, `TestWarmAuthorityAcceptsExternalExtension`: next outer lease rechecks every historical
  byte/head/epoch/projection; lower revision, changed same-revision hash, incompatible generation, missing prefix or projection lag invalidates usable
  cache; external append is visible. Table-drive both cold and warmed variants.
- [x] Write `TestAuthorityViewExpiresWithLease`, `TestAuthorityViewDetachedResults`, `TestNestedAuthorityReadReusesOnlyCurrentLease`: no
  selector/prepared action succeeds after closure; late goroutine borrowing fails, slice edits do not change private state, nested legitimate reads
  perform one integrity pass and a new outer lease performs another. Assert no full state clone through new view reads; legacy snapshot compatibility
  remains until Task 4.
- [x] Run root DB runner with `BT_TEST_PACKAGES='./internal/security' BT_TEST_RUN='Test(WarmAuthority|AuthorityView|NestedAuthorityRead)'`; expect new
  API/behavior failures.
- [x] Implement cache publication under service synchronization, strict cold reconstruction using Task 2 for localstore and unchanged full validation
  for unsupported adapters. Fresh persistent epoch/projection/quarantine checks precede usable views. Reuse only within the active existing fence
  lease; do not add request-level fences. Update internal preparation to private state and preserve private snapshot token/provenance comparisons.
- [x] Rerun targeted tests plus DB runner for `./internal/security/... ./internal/recovery/...`; commit `perf: reuse immutable authority state inside
  validated leases`.

### Task 4: Migrate consumers and publish durable successors safely

**Files:** Modify `Server/internal/auth/{sessions.go,refresh.go,refresh_recovery.go,refresh_recovery_retention.go}`, `Server/cmd/baby-tracker-server/main.go`, `Server/internal/family/{service.go,lifecycle.go,entitlements.go}`, `Server/internal/account/{lifecycle.go,apple_revocation.go}`, `Server/internal/identity/apple_notifications.go`, `Server/internal/export/worker.go`, `Server/internal/realtime/authorizer.go`, `Server/internal/security/{coordinator.go,prepared.go,authority_view.go,state.go,export_snapshot.go,invalidation.go}`; update their existing test files and affected existing security snapshot/capability tests, and add `Server/internal/security/authority_successor_test.go`.
**Interfaces:** Add lease-expiring `AuthorityView.Head() Head` and `AuthorityView.RecoveryEpoch() uint64` metadata selectors returning zero when expired/empty, derived only from the validated private owner; these scalars are not authorization tokens. Change `authorityFamilySlot(state security.State, accountID string)` to `authorityFamilySlot(view security.AuthorityView, accountID string)` preserving return types. Migrate all production `ReadState`/`snapshot.State` consumers and affected tests to callbacks/selectors, including `auth.Service.ReferencedRecoveryKeyIDs(ctx, snapshot)` to accept the already validated `AuthorityView`, then remove `AuthoritySnapshot.State` and the transitional clone. Keep internal prepared-command state private; update snapshot-tampering tests to exercise View/private-token replacement without weakening expected rejection. Keep `ReadState` only as explicit detached diagnostic compatibility API. Add private `Service.publishAuthoritySuccessor(ctx context.Context, prior *validatedAuthority, candidate Candidate, next Head) error` after durable head/projection success; clone once on writes, never mutate published readers.
- [x] Write `TestAuthoritySuccessorPublishesBeforeFenceRelease`, `TestAuthoritySuccessorCASLossFailsClosed`,
  `TestStalePreparedCapabilityCannotCommit`: verify immediate member/device/session revocation on next transaction and affected socket, old view
  unchanged, candidate/head ordering preserved, CAS loser not published, projection failure quarantines and denies, forged copied head/token and
  expired capability rejected.
- [x] Add `TestReadConsumersUseDetachedViews` in existing family/auth tests: identical account scope, membership/entitlement decision and receipt
  replay results without whole-state reads; include concurrent readers/writer and callback cancellation.
- [x] Run root DB runner with `BT_TEST_PACKAGES='./internal/security ./internal/auth ./internal/family ./internal/account ./internal/export
  ./internal/realtime ./internal/identity' BT_TEST_RUN='Test(AuthoritySuccessor|StalePreparedCapability|ReadConsumers)'`; expect missing
  migration/successor behavior.
- [x] Implement successor and migrate bounded consumers without restructuring unrelated modules. On durable CAS success but projection failure
  invalidate/quarantine before release. Recovery reconciliation may replace binding only under coordinated trusted-root/epoch rules; known rollback
  stays denied. Preserve existing invalidation timing even on projection failure.
- [x] Rerun target and full affected DB suites; `rtk rg 'snapshot.State|ReadState\(' Server --glob '*.go' --glob '!**/*test.go'` must show only
  documented explicit diagnostics/compatibility, then commit `perf: use authority selectors across authenticated services`.

### Task 5: Cached key inventory and fresh complete readiness

**Files:** Modify `Server/internal/auth/{refresh_recovery_retention.go,refresh_recovery_retention_test.go}`, `Server/internal/security/{coordinator.go,transaction.go,authority_cache.go,authority_view.go,state.go}`, `Server/cmd/baby-tracker-server/{main.go,main_test.go}`, `Server/internal/testkit/readiness_performance_probe_test.go`; create `Server/cmd/baby-tracker-server/readiness_integrity_test.go` and `Server/internal/security/credential_inventory_test.go`.
**Interfaces:** `Service.ReferencedCredentialKeyIDs(ctx context.Context) (CredentialKeyInventory, error)` opens/reuses a valid view. Inventory is detached and head/epoch bound; authority-only references are built once per validated state. Recovery eligibility is also time dependent, so its cached references require a provider-owned revalidation boundary. Recovery-first dependency produces `auth.RecoveryKeyInventory { SessionToken []string; DeviceContinuity []string }` with data-only `(*auth.Repository).RecoveryKeyCandidates(ctx context.Context) ([]auth.RecoveryKeyCandidate,error)` and conservative raw `Repository.ReferencedRecoveryKeyIDs(ctx)`. Current eligibility uses `(*auth.Service).ReferencedRecoveryKeyIDs(ctx, view security.AuthorityView) (auth.RecoveryKeyInventory,error)` after Task 4 migrates its snapshot argument. Add `SessionToken []string` to `security.CredentialKeyInventory`; inject `RecoveryKeyReferencesResult { Inventory CredentialKeyInventory; RevalidateAt time.Time; Cacheable bool }` and `Options.RecoveryKeyReferences func(context.Context, AuthorityView) (RecoveryKeyReferencesResult,error)` from main to map/union auth references using the supplied validated view without a security→auth import cycle or nested authority-fence acquisition. Query once when validating a new immutable head under its fence, never publish before the receipt projection transaction has committed; pending or failed receipt projection/inventory lookup fails closed. An older inventory alone cannot authorize new-head readiness because it may omit newly required keys; only a freshly verified union covering the new head can be published. Readiness compares the configured session-token ring too. Task5 adds `auth.RecoveryKeyEligibility { Inventory RecoveryKeyInventory; RevalidateAt time.Time; Cacheable bool }` and `Service.ReferencedRecoveryKeyEligibility(ctx, view) (RecoveryKeyEligibility,error)`; existing IDs-only service method delegates while preserving its return contract. Add data/policy-only `auth.NewRecoveryKeyReferences(repository *Repository, mode config.Mode, now func() time.Time) (func(context.Context, security.AuthorityView) (RecoveryKeyEligibility,error), error)` with validated inputs. Its callback and service eligibility method share one private implementation; it never creates an incomplete general auth.Service or acquires an authority fence. Main composes this provider before security.New, without a mutable setter or circular service pointer. The auth provider computes the nearest future effective deadline among retained known candidates, including the self-hosted lineage ceiling. At or beyond that boundary, refresh eligibility under a fresh valid view even at the same head. A required refresh error denies readiness; unknown authority or failed lineage lookup conservatively retains references and marks the recovery result non-cacheable, forcing another query on the next valid lease rather than publishing permanent same-head retention. Refresh once per active lease, without recursive retry loops. No arbitrary TTL or historical-integrity caching is introduced. Ack/cleanup conservatively retain refs until a validated refresh; no second key registry.
- [x] Write `TestReadinessWarmCacheStillRejectsDrift`: warm real readiness, independently alter recovery identity/epoch/head/projection, schema/role
  delegation and configured keys; each fresh check denies. Outage returns unavailable; no TTL success reuse.
- [x] Write `TestReceiptInventoryCannotRetireRequiredKeys`: outstanding receipts retain derivation/output/device keys after session rotation;
  retirement racing a readiness check never exposes a false-safe inventory. Conservative retention until next validated refresh is acceptable. Assert
  key-set mutation cannot alter next result and readiness does not rescan historic sessions on every request. Add fake-clock same-head expiry and new-head receipt cases: deadlines trigger required refresh; failed refresh denies, unresolved lineage results requery at the next lease and can later release expired references; every new head obtains a complete fresh union before publication.
- [x] Run root DB runner with `BT_TEST_PACKAGES='./cmd/baby-tracker-server ./internal/security ./internal/auth'
  BT_TEST_RUN='Test(ReadinessWarmCache|ReceiptInventory|RecoveryKeyEligibility)'`; expect missing cached inventory behavior.
- [x] Implement inventory on successor/cold publication and conservative retirement invalidation. Preserve base recovery/schema/role checks and hosted
  gates; measure `applicationReadiness.Check`, not only `databaseReadiness` or `Gate.Check`. Reuse nested proofs solely within the existing valid
  lease.
- [x] Run existing `TestApplicationReadinessRejectsMissingCredentialKeysAndHostedGates`, new tests and complete application command suite; capture
  opt-in readiness phase costs, commit `perf: derive credential inventory once per validated authority state`.

### Task 6: Family-indexed realtime routing and notification lookup

**Files:** Modify `Server/internal/realtime/{hub.go,hub_test.go,postgres.go,postgres_test.go,authorizer.go,authorizer_test.go}`.
**Interfaces:** Add private `Hub.byFamily map[string]map[*lease]struct{}` and `func (hub *Hub) HasRegisteredFamily(familyID string) bool`. Keep lease registry authoritative. Add `DecodeNotificationForFamily(payload []byte, instanceID string, recoveryEpoch uint64, registered func(string) bool) (Hint, error)`; existing map API can delegate for compatibility. `RegisteredLeases() []AuthorizationLease` remains a bounded consistent reconnect snapshot.
- [x] Write `TestFamilyIndexLifecycleAndConcurrentPublish`: reservation unbound is unroutable, binding/release/expiry/revocation/shutdown update both
  registries atomically; racing release/Publish maintains zero leaked membership, no cross-family writes, existing account/device/process quotas
  unchanged.
- [x] Write `TestIndexedPublishTargetsOnlyFamily`, `TestNotificationLookupRejectsForeignDomains`: candidate selection work depends on target leases,
  high-water hints coalesce, slow target consumer does not block another family, writes occur outside mutex; malformed/invalid UUID or
  sequence/instance/epoch/family mismatch and unregistered hints reject without creating registrations. Reconnect uses reauthorized snapshots;
  invalidation still closes sockets.
- [x] Run root DB runner `BT_TEST_PACKAGES='./internal/realtime' BT_TEST_RUN='Test(FamilyIndex|IndexedPublish|NotificationLookup)'`; expect absent
  index/lookup failures.
- [x] Implement index mutations in existing hub lock paths and targeted selection outside-lock bounded writes; replace per-notification
  registered-family map construction. Keep periodic sweep and one valid authority view per CheckMany sweep.
- [x] Run full realtime suite with race via DB runner, commit `perf: route live hints through a family lease index`.

### Task 7: Own a disposable self-hosted full-composition timing harness

**Files:** Create `Server/scripts/sync-performance.sh`, `Server/scripts/test-performance-runner.sh`, `Server/scripts/lib/disposable-sync.sh`, `Server/cmd/sync-performance-client/main.go` and narrow fault-control `main_test.go`; modify `Server/scripts/protocol-e2e.sh` only to share exact existing resource setup safely; create `Server/cmd/baby-tracker-server/performance_composition_test.go` and identitytest-tagged `performance_composition_identitytest_test.go`.
**Interfaces:** New runner (does not exist yet): root `rtk proxy env BT_SYNC_PERF_PROBE=1 bash Server/scripts/sync-performance.sh --output /path/to/baby-sync-perf --duration 30 --total-seconds 1200`. Own all DB/app/TLS/filesystem volumes and refuse external DSNs/origins. Share setup via `start_disposable_sync(mode, build_profile)` / `stop_disposable_sync()` shell functions; explicit selfhosted authority uses production localstore/recovery/schema/runtime/projector adapters and real applicationOptions followed by production maintenance-pool principal verification and conditional RecoveryReadiness attachment. Start the actual cleanup worker with its recovery Gate.Check admission. Identity proof fixture remains identitytest only; no credential/session/authority shortcuts. Normal build for timing, no race flag.
- [x] Write runner failure-injection tests: absent opt-in refuses before starting; invalid/external destination refuses; SIGINT, app start failure and
  timeout remove exact owned containers/volumes; logs/artifacts redact secrets. Composition test asserts actual readiness, default limiter, delivery
  observer/queue, listener, proxy upgrade and two authenticated sockets are present; disabling observer makes hint gate fail. Separately assert enabled recovery capability/route admission, maintenance function privilege and actual cleanup worker readiness; /readyz alone cannot prove these paths.
- [x] Run new `rtk proxy bash Server/scripts/test-performance-runner.sh` after test file exists, plus root DB runner
  `BT_TEST_PACKAGES='./cmd/baby-tracker-server' BT_TEST_RUN='TestPerformanceComposition'`; expect the absent composition/runner assertions to fail.
- [x] Implement explicit selfhosted mode with trusted localstore mounted to owned disposable volumes; retain hosted protocol runner defaults and
  pinned container identities. Setup uses real transitions with two accounts/devices per family, deterministic fixtures, observed head/epoch and
  existing care history. The performance client coordinates load and collects server metrics, never claims peer store/UI timing itself.
- [x] Rerun new runner tests and `rtk make server-protocol-e2e`; require existing image/schema/cleanup/completion proofs plus selfhosted readiness and
  upgraded sockets. Commit `test: add disposable complete sync performance composition`.

### Task 8: Measure the actual Swift peer durable and observable path

**Files:** Create `Packages/BabyTracker/Tests/FamilySyncClientE2ETests/ForegroundLatencyE2ETests.swift`; modify `TwoCaregiverSyncE2ETests.swift` in that directory to share its real SettingsFeature `Phone` composition helper without copying it; modify `Server/scripts/sync-performance.sh`, narrow case/marker/count controls in `Server/scripts/test-performance-runner.sh`, and `Makefile` exact test total as needed. Execution ruling permits narrow internal `SettingsFeature/FamilySyncComposition.swift` live-failure injection (default real channel unchanged) and its task-owned actual wiring assertions; no public make option, authorization hook or timer change.
**Interfaces:** Add an opt-in serialized Swift suite using existing pinned TLS/session setup and app composition against the complete Task 7 server, including recovery maintenance readiness and its worker; `measureForegroundSaveToPeer(owner: Phone, peer: Phone, samples: Int) async throws -> ForegroundLatencyReport` in the test target, with per-sample durable and observable durations from Task 1. Share test-only helper visibility; no production test-only authorization hooks. Require explicit runner success marker `foreground-latency proof: complete` and artifact counts.
- [x] Write `foregroundLiveHintAppliesBeforeFallback`, `foregroundDisabledHintsFailLatencyGate`,
  `foregroundReconnectAndRenewalHaveSeparateDistribution`: real local writer save starts timer, no manual owner `sync()` push, peer journal/store
  commit and observable publication end timers; >=200 caught-up established-socket observations, zero lost/duplicate/cross-family records, no auth
  churn. Disabled live hint must fail within one-second acceptance while fallback is 20 seconds away; retain a bounded hint-only diagnostic poll
  override separately.
- [x] Run new suite through the new disposable runner, which supplies `BABY_TRACKER_E2E_BASE/STATE/CERT/INSTANCE`; a standalone filtered Swift run can
  silently skip and is not evidence. Expect missing measurement/gate failures before implementing helpers.
- [x] Implement real-store and Observation callbacks plus monotonic same-process clocks. Pair sockets before warm-up; separate initial dial, dropped
  socket, foreground return, access renewal, offline backlog, snapshot/import and background samples. Emit percentiles of complete pairs; HTTP
  latencies remain separate fields. Add signed-out owner negative case; do not change timers.
- [x] Rerun complete opt-in suite; require durable and observable p95 <1 s for healthy two-phone foreground path and the disabled-hint negative proof.
  Standalone >=200-sample measurement may take up to120 seconds because unchanged300ms debounce already consumes60 seconds before network/application work; this is not the Task9 capacity-cell window, whose30–60-second measurement/120-second total bound remains. Simulator/UI rendering and physical-device visible updates remain separate acceptance evidence, disclose limits. Commit `test: measure foreground
  save to durable peer application`.

### Task 9: Bounded capacity matrix and baseline comparison

**Files:** Modify `Server/cmd/sync-performance-client/main.go`, narrow worker controls in existing `main_test.go`, `Server/scripts/sync-performance.sh` and supervisor cleanup controls in `Server/scripts/test-performance-runner.sh`, `ForegroundLatencyE2ETests.swift`; create untagged `Server/cmd/sync-performance-client/report.go`, explicit-refusal `!identitytest` entrypoint `main_disabled.go`, `Server/cmd/sync-performance-client/report_test.go`, separate shared-helper `CapacityMatrixE2ETests.swift`, and `docs/verification/2026-10-01-sync-performance-results.md`; add `Server/scripts/lib/performance_control.py` for bounded control/checkpoint aggregation/teardown, and narrow existing diagnostics/security/localstore/records/httpapi/server-composition hooks for actual fence/integrity/transaction/pool/runtime measurements with immutable observations emitted after protected scopes; update `Makefile` by exact newly registered tests as needed.
**Interfaces:** Define `MatrixCell { Families, Concurrency, RequestedEvents, ActualEvents int; Contention string }` and `CellReport` with separate request, durable-peer and observable-peer distributions, successful QPS, failure/retry/rate-limit/timeouts and completion state. Test report serialization includes source/image/build/environment and no sensitive IDs. Swift owns the per-cell peer workers: one real app-composition writer and reader/store pair per family, hence exactly two authenticated foreground sockets. It signs in the synthetic fixture subjects, provisions families and exports only to an owned mode0600 ephemeral credential manifest for Go request workers. Go emits bounded cell-start/finish and per-operation completion JSON records over stdout, receiving bounded work commands over stdin. Swift is the sole deterministic five-operation scheduler (two empty pulls, two caught-up reads, one save/peer apply), launches the Go child for that cell, and maintains at most the cell's concurrency active complete operations across both workers. Empty-pull and caught-up-read categories use the same existing no-mutation POST /v1/sync at the current cursor; the latter additionally checks real reader/store catch-up. Report their wire equivalence and actual nonempty/lagging responses explicitly. Swift returns complete-pair samples keyed only by ephemeral operation IDs to the cell report; Go samples request load separately. Keep subjects/credential manifest out of report artifacts and clean it with owned resources. Enforce the actual cell wall boundary through an owned private bounded control channel: supervisor arms an118-second deadline and ACKs before Swift begins, matches finish ordinals only after cleanup, and reaps the exact owned child group. Buffered stdout remains diagnostic. Record actual setup/measurement/cleanup elapsed even on failed cells. After actual family-ready/head observation, a proven remaining-rotation upper bound under documented normal pacing may produce `paced_setup_budget_insufficient`; label this modeled setup-budget result separately from observed refusals and measured capacity. New runner's `--baseline-source <clean-source-root>` builds unchanged baseline from an isolated supplied root; never rewrites current checkout or launches production load.
- [ ] Write `TestMatrixBoundsAndHonestFailures`, `TestPercentilesUseOnlyCompletedPairsWithFailuresReported`: enumerate 27 cells; enforce 120 s/cell
  and 1200 s/global including setup, report unfinished cells; failures cannot count as success, over-quota requests are refusals, no empty percentile
  success, actual event counts retained. Concurrency > families is labelled same-family contention; keep all isolated-family combinations.
- [ ] Run from `Server`: `rtk go test -count=1 ./cmd/sync-performance-client -run 'Test(MatrixBounds|Percentiles)'`; expect missing reports/budgets.
- [ ] Implement >=248/512/2048 valid committed transitions in setup with exact actual sizes (family/auth setup may exceed requested floor). Seed
  existing care history; each family has two authenticated foreground sockets, label quota-limited incomplete cells. Warm-up then 30–60 s
  deterministic mix: 40% empty pulls, 40% caught-up reads, 20% one-record care saves/peer applies; measure a fixed 100-change bounded backlog
  separately. At least 200 successes where attainable; stop at bounds without erasing findings.
- [ ] Record pool acquire waits, transaction/family/security fence waits, DB connections, CPU, allocations/RSS, network/filesystem bytes and O(N)
  integrity-pass duration with exact source/build environment. Compare unchanged baseline and new build with identical fixture manifest and normal
  builds; existing synthetic probe is historical HTTP evidence only.
- [ ] Run new runner twice against disposable baseline/current sources under the same 20-minute bound per diagnostic run; publish cell results and
  supported-envelope recommendation. If O(N) integrity prevents target, stop optimization claim and document separate security-contract decision
  required. Commit `test: report bounded sync capacity envelope`.

### Task 10: Whole-branch correctness and acceptance evidence

**Files:** Update only `docs/verification/2026-10-01-sync-performance-results.md` and existing project tracker after reconciling actual changes; inspect all scoped diff paths.
**Interfaces:** No new product API. Consume completed task reports, source SHA and actual harness completion artifacts.
- [ ] Run root `rtk make server-test`, `rtk make server-build`, `rtk make test`, `rtk make server-protocol-e2e`, `rtk make build`; affected UI gate
  uses the same xcodebuild test action/scheme with `-only-testing:BabyTrackerUITests/HomeSyncRecoveryUITests` on mandatory shared destination `platform=iOS Simulator,id=<simulator-udid>`. Execution ruling narrows the broad `make test-ui` selector because that broader suite deletes the shared simulator persistent App Group store; Home recovery fixtures are in memory. Report affected tests only, not full UI-suite acceptance. Missing/incompatible shared device is a blocker; no creation/cloning/runtime installation. Record explicit blockers/skips and do not substitute filtered Swift tests for
  literal-count gate.
- [ ] Verify warmed/cold adversarial security matrix covers corruption/deletion/replacement, rollback/external append/CAS, stale capability/mutable
  escape, concurrent readers/writer, projection/role/key/quarantine/epoch drift, immediate membership/device/session revocation, notification
  isolation, cancellation and lock cleanup. Race checks are correctness only.
- [ ] Review final diff with fresh reviewer under selected execution workflow; reject undocumented fence widening, cache-authority shortcuts,
  request-to-peer latency substitution, resource leaks or silently skipped E2E gates. Confirm protocol schema remains compatible with companion
  receipt migration.
- [ ] Record measured target cells, unsupported/unfinished cells, rate-limit/backpressure behavior, global-writer/O(N) cost and fixed total
  pool/listener/socket budget justified by data. No automatic pool increase; supported envelope is a rollout decision.
- [ ] Document physical-device local-save/durable/visible UI evidence still needed, background opportunism and source/deployed/installed distinctions.
  Deployment needs separately reviewed SHA/image, backups, schema22→reviewed-source migration/compatibility and rollback constraints; after authorized
  deployment, verify deployed identity/schema/readiness/socket/aggregate health. End with Changes, Verification, Remaining risks and Blocked on you;
  commit scoped evidence only if authorized.


## User-requested sealed checkpoint — 2026-10-01

No further tests or builds are authorized at this checkpoint. Tasks1–8 are reviewed; Task9 original findings are addressed, with two subsequent reporting fixes being sealed without test execution. Task10 execution gates completed on a34fe2f, including literal1191, shared-simulator app build, affected server races, hosted protocol, self-hosted recovery/metrics and2Home UI tests. The initial fullserver gate failed and subsequent scoped passes are not one full-suite green. Whole-branch review is deferred; no capacity or release acceptance, integration, push or deployment is declared. Resume from `docs/superpowers/handoffs/2026-10-01-sync-reliability-sealed.md`.
