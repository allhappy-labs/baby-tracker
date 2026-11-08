# Family Sync iOS Client Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an optional, realm-isolated iOS family-sync client that preserves immediate local-only tracking while safely onboarding, importing, synchronizing, and revoking one shared family.

**Architecture:** `BabyProtocol` is the pure Swift representation of the versioned Plan 1/2 HTTP/OpenAPI contract; `BabyStore` owns versioned SwiftData, active-realm predicates, a process-safe fenced lease/transition-generation port, atomic base-plus-overlay materialization, and import transitions. `BabySync` owns transport, Keychain sessions, retry, and WebSocket lifecycle, and consumes that store port without a reverse `BabyStore` → `BabySync` dependency; Settings and the app target receive presentation models through injection.

**Tech Stack:** Swift 6.2, Swift Testing, SwiftData with explicit `VersionedSchema` migration, Foundation/URLSession/WebSocket, Keychain Services, SwiftUI, AuthenticationServices, BackgroundTasks, UserNotifications, App Groups, Xcode/iOS simulator and signable-device gates, Go/OpenAPI contract fixtures from Plans 1 and 2.

**Spec:** `docs/superpowers/specs/2026-09-19-family-sync-server-design.md`; `docs/superpowers/plans/2026-09-20-family-sync-server-foundation.md`; `.superpowers/planning/full-beta-decomposition.md`; `.superpowers/planning/ios-sync-architecture.md`

## Global Constraints

- Keep local-only use fully functional without an account, network request, Keychain session, or shared outbox.
- Use Swift 6 and support iOS 26/macOS 26; preserve `BabySchema.makeContainer` as the sole serialized `ModelContainer` opening path.
- Add `BabyProtocol` (the P3 decomposition’s authoritative name; the architecture map’s `BabySyncProtocol` label refers to this same single target) as Foundation/BabyCore-only protocol values; `BabyStore` imports `BabyProtocol`, `BabySync` imports `BabyCore`, `BabyProtocol`, and `BabyStore`, and feature modules never construct HTTP DTOs or SwiftData contexts.
- Consume Plan 1 identity/family endpoints and Plan 2 `POST /v1/sync`, `GET /v1/sync/snapshot`, and `/v1/live` fixtures exactly; open `/v1/live` only when `/v1/server` advertises `live-hints-v1`, send exact capabilities `records-v1,live-hints-v1` plus subprotocol `baby-tracker.v1`, and remain polling-only otherwise. Do not replay `ChangeLogEntry` or invent replacement endpoints, payloads, cursor semantics, or timer operations.
- A `RealmID` is either permanent `.local` or `.shared(canonicalOrigin:serverInstanceID:familyID:)`; every entity, selected-child value, base, outbox row, cursor, checkpoint, endpoint selection, and lease is realm-scoped.
- Store only access/refresh/device session material in Keychain, keyed by canonical origin, server instance, family, account, device, and epoch; use App Group `group.com.example.babytracker` only for non-secret shared persistence, and never put secrets in SwiftData, UserDefaults, logs, snapshots, launch arguments, UI-test fixtures, or metrics.
- Before a family exists, retain the P1 session only under `AccountSessionKey(canonicalOrigin,serverInstanceID,accountID,deviceID,recoveryEpoch)`; after membership, atomically rekey it to `FamilySessionKey(canonicalOrigin,serverInstanceID,familyID,accountID,deviceID,recoveryEpoch)`. Promotion records membership readiness; P3.4 alone binds the shared realm after import. Checkpoints prevent duplicate secret items.
- Retain P1’s `device_continuity:{device_id,credential}` binding in Keychain across app restart, access expiry, and refresh-reuse reauthentication. Fresh Apple proof supplies it only to the selected origin/instance; `device_continuity_invalid` or `device_continuity_required` blocks frozen-mutation replay and surfaces a sync issue rather than silently allocating/retargeting a device.
- Apple credential state `.revoked` or `.notFound` signs out the affected account immediately, including caregivers and self-host sessions: durably deny ordinary sync, stop transport, clear access/refresh credentials, and purge its shared cache under D10. Keep only the origin/account-bound Keychain device-continuity proof for fresh Apple authentication; it cannot authorize ordinary sync. Preserve server family data and other accounts. Credential-state loss never sends `/logout` or uses the erased session: hosted Apple server notifications revoke server sessions, while self-host relies on immediate client denial plus manual Apple-account Settings guidance and requires fresh Apple proof before sync resumes. Reserve `/logout` and device-binding revocation for explicit disconnect/account deletion.
- `BabyStore` owns an App-Group fenced lease port before import/sync consumers: use a short-held OS advisory lock around durable lease CAS and cursor/import commits, persist `(scope, ownerID, generation, expiry)`, never await network while holding it, require renewal before each request/commit, and reject a resumed stale owner whose generation was superseded. The same account-scoped lease serializes refresh rotation; a durable pre-send refresh-attempt marker prevents an expired-owner takeover from replaying a possibly consumed parent refresh token.
- Parse custom servers as HTTPS origins with no credentials, query, fragment, path, redirect, or release TLS bypass. An authenticated endpoint is immutable; a changed instance at a known origin requires confirmation before credentials or data are sent.
- Resolve D6 with the Plan 1 nonce vectors: concatenate UTF-8 `baby-tracker-auth-v1`, byte `0x00`, UTF-8 canonical origin, byte `0x00`, lower-case UUID instance ID, byte `0x00`, and the challenge's exact unpadded-base64url UTF-8 text; SHA-256 that byte sequence and emit lower-case hexadecimal. Never decode the challenge for hashing or interpret the printed `\\0` as two literal characters.
- Resolve D10 explicitly: import moves rows from the permanent local realm into the selected shared realm. Disconnect, revocation, leave, deletion scheduling, and account deletion purge that shared realm and never silently recreate a local duplicate; the permanent local realm itself remains, and the confirmation copy directs owners to the server export flow when they need a copy.
- Preserve the exclusions: no billing UI, Android/web client, E2EE, attachment bytes/storage/UI, audit-log browser, operator web console, Redis, Kubernetes, multi-region writer, or multiple-active-family beta UI/API.
- Keep production connections HTTPS-only, allow hosted Associated Domains only for the approved hosted domain, and add no production debug origin or TLS exception. Self-hosted links use confirmed custom-scheme/manual-code handoff.
- Use conventional commits, stage only task-scoped files, run `rtk`-prefixed commands, retain the literal package-test-count guard, and update `EXPECTED_TESTS` only in the commit that adds tests.

## Plan 1/2 Contract Inputs

Plan 3 begins only after the following checked-in contract fixtures and OpenAPI operations exist. The iOS client decodes tolerant response objects but emits only these allowlisted v1 request shapes.

| Producer | Contract consumed by this plan |
| --- | --- |
| P1.3/P1.4 | `GET /v1/server` returns `ServerInfo` with active capabilities, `apple_authorization_mode`, effective limits, source version/URL, instance, origin, and epoch. Apple exchange uses P1 `AppleExchangeRequest` with optional account-qualified `device_continuity:{device_id,credential}`; every `SessionBundle` returns its fenced `recovery_epoch`, while `device_credential` is returned once. Fresh proof reuses a device only after the P1 HMAC credential plus the same verified account/origin/instance; `device_continuity_invalid` has no fallback. The exact D6 nonce vectors are authoritative. |
| P1.5/P1.6 | Family create/join, invitation confirmation, member/lifecycle operations, paged active/former actor labels, and stable error envelopes come from P1 OpenAPI/fixtures. Link receipt is non-redeeming until the authenticated confirmation request. |
| P2.1/P2.2 | `WireMutation`, `Outcome`, `JournalChange`, `SnapshotEntity`, `LocalDate`, timer operations including `timer_attach_bottle`, canonical hashes, normalized baby candidates, and import manifests use the P2 v1 fixtures unchanged. |
| P2.3 | `POST /v1/sync` request `{protocol_version, capabilities, cursor, mutations, page_token}` and response `{server_instance_id, recovery_epoch, outcomes, changes, high_water, next_page_token}`; `GET /v1/sync/snapshot` returns P2’s fixed-snapshot pages. |
| P2.5/P2.6 | Resolver/mapping/conflict endpoints are P2’s import contract: `ImportResolveRequest.import_mode` is immutable from chunk 0 as `preserve_local_ids` for create-family or `match_shared` for join; owner conflict `choice` is `use_existing` or `keep_local` with nullable `canonical_baby_id`; final mappings are injective within one import and duplicate local name/date candidates for one shared baby return `ambiguous_local_babies`. `/v1/live` is discoverable only through advertised `live-hints-v1`, requires exact `records-v1,live-hints-v1`, and emits only `{family_id,cursor}` hints. Without that advertisement the client remains polling-only. Hints, push wakes, and reconnects trigger HTTPS sync, never local state application. |

## Review Focus

- **`test_stableRevocationPurgesAfterAuthorizationFailure` in P3.5:** a locally stale session must not retain shared material after the server’s committed authorization denial, even when a live-close notification was lost.
- **`test_fixedHighWaterCursorWaitsForAllPages` in P3.5:** an outcome cursor or partial page must not advance the durable cursor beyond journal changes that have not committed locally.
- **`test_uncertainStartDoesNotDispatchOrRetargetDependentStop` in P3.2:** after a lost start receipt, a side/bottle/stop intent remains locally visible and dependency-blocked rather than acting on another caregiver’s timer.
- **`test_interruptedTransitionNeverExposesMixedRealmRows` in P3.1 and `test_importRewriteLeaseExcludesWidgetWriter` in P3.4:** app/widget/intent writes during migration, import, snapshot, or realm switching must not expose half-moved rows.
- **`test_recoveryEpochDoesNotReuploadAcknowledgedMutation` in P3.5:** an old restored base must be replaced and a previously acknowledged mutation must stay non-dispatchable, preventing the client from reviving data after recovery.

---

### Task 1: Protocol target, copied-store migration, and realm-scoped persistence (P3.1)

**Files:**

- Modify: `Packages/BabyTracker/Package.swift`
- Create: `Packages/BabyTracker/Sources/BabyProtocol/{WireModels,SyncDTOs,ImportDTOs,ProtocolFixtureLoader}.swift`
- Create: `Packages/BabyTracker/Sources/BabyCore/LocalDate.swift`
- Create: `Packages/BabyTracker/Sources/BabyStore/Models/{SchemaV1,SchemaV2,BabyStoreMigrationPlan,LegacyMigrationBridge,RealmID,AccountLeaseKey,AccountAccessState,RefreshAttemptState,SyncOutboxEntry,CanonicalEntityBase,SyncCursorState,ImportCheckpoint,RealmTransitionCheckpoint,CaregiverAttribution,FencedLease}.swift`
- Create: `Packages/BabyTracker/Sources/BabyStore/{AppGroupBootstrapLock,AppGroupFencedLease,AppGroupWriteBoundary,AccountAccessStore}.swift`; `scripts/build-presync-fixture.sh`; `Packages/BabyTracker/Tests/BabyStoreTests/Support/PreSyncFixtureVerifier.swift`
- Create: `Packages/BabyTracker/Sources/BabyStoreLeaseProbe/main.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/Models/{Schema,Baby,FeedEntry,SleepEntry,DiaperEntry,ActiveTimer,ChangeLogEntry}.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/{BabyStore,BabyStore+Babies,BabyStore+Queries}.swift`
- Modify: `Packages/BabyTracker/Sources/BabyFeature/SelectedBabyStore.swift`
- Create: `Packages/BabyTracker/Tests/BabyProtocolTests/{ProtocolFixtureConformanceTests,LocalDateTests}.swift`
- Create: `Packages/BabyTracker/Tests/BabyStoreTests/{SchemaMigrationTests,RealmIsolationTests,RealmTransitionTests,AccountLeaseKeyTests,AppGroupFencedLeaseTests}.swift`
- Create: `Packages/BabyTracker/Tests/BabyStoreTests/Fixtures/PreSyncAppGroupStore.sqlite`
- Create: `Packages/BabyTracker/Tests/BabyStoreTests/Fixtures/PreSyncAppGroupStore.sqlite-wal`
- Create: `Packages/BabyTracker/Tests/BabyStoreTests/Fixtures/PreSyncAppGroupStore.sqlite-shm`
- Create: `Packages/BabyTracker/Tests/BabyStoreTests/Fixtures/PreSyncFixtureManifest.json`

**Interfaces:**

- Consumes: P2 `WireMutation`, `SyncResponse`, `SnapshotResponse`, `ImportResolverResponse`, and their checked-in v1 JSON fixtures; the server owns their JSON names and canonical semantics.
- Produces:

```swift
public enum RealmID: Codable, Sendable, Hashable {
    case local
    case shared(canonicalOrigin: String, serverInstanceID: UUID, familyID: UUID)
}

public struct LocalDate: Codable, Sendable, Hashable, Comparable {
    public let iso8601: String
    public init(_ iso8601: String) throws
}

@MainActor public protocol RealmStore {
    var activeRealm: RealmID { get }
    func beginTransition(to realm: RealmID, checkpointID: UUID) throws
    func commitTransition(checkpointID: UUID) throws
    func recoverIncompleteTransition() throws
}

// BabyStore-owned non-secret identity; no dependency on Keychain or BabySync.
public struct AccountLeaseKey: Codable, Sendable, Hashable {
    public let canonicalOrigin: String
    public let serverInstanceID: UUID
    public let accountID: UUID
    public let deviceID: UUID
    public init(canonicalOrigin: String, serverInstanceID: UUID, accountID: UUID, deviceID: UUID)
}

public enum LeaseScope: Codable, Sendable, Hashable {
    case realm(RealmID)
    case account(AccountLeaseKey)
}

public struct FencedLeaseToken: Codable, Sendable, Hashable {
    public let scope: LeaseScope
    public let ownerID: UUID
    public let generation: Int64
    public let expiresAt: Date
}

public enum FencedLeaseError: Error, Sendable, Equatable { case staleOwner, expired, cancelled }

@MainActor public protocol FencedLeaseStore: AnyObject {
    func acquire(scope: LeaseScope, ownerID: UUID, now: Date, ttl: TimeInterval) throws -> FencedLeaseToken?
    func renew(_ token: FencedLeaseToken, now: Date, ttl: TimeInterval) throws -> FencedLeaseToken
    func withFencedCommit<T>(_ token: FencedLeaseToken, now: Date, _ body: () throws -> T) throws -> T
    func cancel(_ token: FencedLeaseToken) throws
}

@MainActor public protocol SyncPersistence: AnyObject {
    func accountAllowsOrdinarySync(_ key: AccountLeaseKey) throws -> Bool
    func nextDispatchBatch(limit: Int) throws -> [WireMutation]
    func markDispatched(mutationIDs: [UUID]) throws
    func applySyncPage(_ page: SyncResponse) throws
    func applySnapshotPage(_ page: SnapshotResponse) throws
    func finishSnapshot(snapshotID: UUID) throws
    func purgeSharedRealm(_ realm: RealmID) throws
}

public struct RealmWriteContext: Sendable, Equatable {
    public let realm: RealmID
    public let transitionGeneration: Int64
}
public enum RealmWriteError: Error { case retryAfterTransition, wrongRealm }

// Non-secret refresh coordination state owned by BabyStore from P3.1.
public struct RefreshAttempt: Codable, Sendable, Equatable {
    public let attemptID: UUID
    public let credentialVersion: UUID
    public let ownerGeneration: Int64
    public let startedAt: Date
}
public enum RefreshAttemptState: Codable, Sendable, Equatable {
    case idle
    case inFlightUncertain(RefreshAttempt)
}

@MainActor public extension BabyStore {
    func captureWriteContext() throws -> RealmWriteContext
    func withRealmWrite<T>(expected: RealmWriteContext, _ body: () throws -> T) throws -> T
    func accountAllowsOrdinarySync(_ key: AccountLeaseKey) throws -> Bool
    func markAccountSignedOut(_ key: AccountLeaseKey) throws
    func markAccountAuthenticated(_ key: AccountLeaseKey, credentialVersion: UUID) throws
    func refreshAttemptState(for key: AccountLeaseKey) throws -> RefreshAttemptState
    func beginRefreshAttempt(for key: AccountLeaseKey, credentialVersion: UUID, using token: FencedLeaseToken, now: Date) throws -> RefreshAttempt
    func clearRefreshAttemptAfterProvenNoSend(_ attemptID: UUID, for key: AccountLeaseKey, using token: FencedLeaseToken) throws
    func finishRefreshAttempt(_ attemptID: UUID, for key: AccountLeaseKey, using token: FencedLeaseToken, installingCredentialVersion: UUID, installCredentials: () throws -> Void) throws
}
```

- [ ] **Step 1: Write failing protocol, copied-store, and realm-isolation tests (2–5 minutes)**

```swift
@Test func test_copiedV1StoreMigratesToLocalRealmWithoutLegacyOutboxReplay() throws {
    let store = try SchemaMigrationFixture.openCopiedPreSyncStore(named: "PreSyncAppGroupStore")
    #expect(try store.babies(in: .local).map(\.id) == SchemaMigrationFixture.expectedBabyIDs)
    #expect(try store.pendingOutbox(in: .local).isEmpty)
    #expect(try store.importSummary().convertedBirthDates == ["2019-11-03"])
    #expect(try store.baby(SchemaMigrationFixture.babyID).photoData == SchemaMigrationFixture.photoBytes)
    #expect(try store.activeTimers(for: SchemaMigrationFixture.babyID).map(\.id) == [SchemaMigrationFixture.timerID])
    #expect(try store.legacyChangeLog().map(\.id) == [SchemaMigrationFixture.changeLogID])
}

@Test func test_predecessorBuiltFixtureOpensAndMigrates() throws {
    let fixture = try PreSyncFixtureVerifier.checkedInFixture()
    #expect(fixture.manifest.producerSHA == Fixture.acceptedPredecessorSHA)
    #expect(try fixture.verifyDigestManifest())
    #expect(try SchemaMigrationFixture.openAndMigrate(fixture).entityKinds == [.baby, .feed, .sleep, .diaper, .activeTimer, .changeLog])
}

@Test func test_interruptedTransitionNeverExposesMixedRealmRows() throws {
    let store = try BabyStore.fixture(activeRealm: .local)
    let target = RealmID.shared(canonicalOrigin: "https://sync.example", serverInstanceID: UUID(uuidString: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")!, familyID: UUID(uuidString: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")!)
    try store.beginTransition(to: target, checkpointID: UUID(uuidString: "cccccccc-cccc-cccc-cccc-cccccccccccc")!)
    #expect(try store.babies().allSatisfy { $0.realmID == .local })
}

@Test func test_suspendedStaleOwnerCannotCommitAfterOtherProcessTakesGeneration() throws {
    let harness = try TwoProcessLeaseHarness.make(scope: .realm(SchemaMigrationFixture.sharedRealm))
    let old = try harness.launchProbe(ownerID: SchemaMigrationFixture.ownerA, action: .acquireThenSuspend)
    let new = try harness.runProbe(ownerID: SchemaMigrationFixture.ownerB, action: .takeExpiredLeaseAndCommit)
    try old.resume(action: .fencedCommit)
    #expect(new.result == .committed)
    #expect(old.result == .staleLease)
}

@Test func test_accountLeaseIdentityRoundTripsWithoutSessionDependency() throws {
    let key = AccountLeaseKey(canonicalOrigin: "https://sync.example", serverInstanceID: SchemaMigrationFixture.serverInstanceID, accountID: SchemaMigrationFixture.accountID, deviceID: SchemaMigrationFixture.deviceID)
    let encoded = try JSONEncoder().encode(LeaseScope.account(key))
    #expect(try JSONDecoder().decode(LeaseScope.self, from: encoded) == .account(key))
}
```

- [ ] **Step 2: Run the focused RED tests and record the expected missing-schema/API failures (2–5 minutes)**

Run: `rtk swift test --package-path Packages/BabyTracker --filter 'SchemaMigrationTests|RealmIsolationTests|AccountLeaseKeyTests|AppGroupFencedLeaseTests|ProtocolFixtureConformanceTests'`

Expected: FAIL because `RealmID`, `AccountLeaseKey`, `LocalDate`, the migration-plan container overload, a faithful V1 fixture builder, a cross-process fenced lease, and P2 DTO fixture decoding do not exist.

- [ ] **Step 3: Implement the smallest versioned schema and realm boundary that satisfies the fixtures (2–5 minutes)**

```swift
public enum BabyStoreMigrationPlan: SchemaMigrationPlan {
    public static var schemas: [any VersionedSchema.Type] { [SchemaV1.self, SchemaV2.self] }
    public static var stages: [MigrationStage] { [migrateV1toV2] }
    public static let migrateV1toV2 = MigrationStage.custom(fromVersion: SchemaV1.self, toVersion: SchemaV2.self) { context in
        try LegacyMigrationBridge.captureV1Rows(from: context)
    } didMigrate: { context in
        try LegacyMigrationBridge.applyCapturedRowsToV2(in: context, defaultRealm: .local)
    }
}

public static func makeContainer(
    schema: Schema,
    migrationPlan: (any SchemaMigrationPlan.Type)? = nil,
    configurations: [ModelConfiguration]
) throws -> ModelContainer {
    openLock.lock(); defer { openLock.unlock() }
    return try ModelContainer(for: schema, migrationPlan: migrationPlan, configurations: configurations)
}

public static func container(inMemory: Bool) throws -> ModelContainer {
    let schema = Schema(versionedSchema: SchemaV2.self)
    let configuration = inMemory
        ? ModelConfiguration(schema: schema, isStoredInMemoryOnly: true)
        : ModelConfiguration(schema: schema, groupContainer: .identifier(appGroupID))
    return try makeContainer(schema: schema, migrationPlan: BabyStoreMigrationPlan.self, configurations: [configuration])
}

public func babies(in realm: RealmID? = nil) throws -> [Baby] {
    let requiredRealm = realm ?? activeRealm
    return try context.fetch(FetchDescriptor<Baby>(predicate: #Predicate { $0.realmKey == requiredRealm.encodedKey && !$0.isTombstoned }))
}
```

Build the accepted predecessor SHA and have its actual `BabySchema` create the fixture in an App Group-compatible directory with a baby, every record, active timer, nonempty `photoData`, and `ChangeLogEntry`, then close it. `build-presync-fixture.sh` records producer SHA/toolchain/model metadata and cryptographic digests. Include WAL/SHM only for an intentionally crash-consistent WAL fixture. `SchemaV1` must match that artifact's metadata before it may drive migration; a newly recreated declaration alone is not evidence. `SchemaV2` adds realm/base/outbox/cursor/checkpoint/attribution. In `willMigrate`, capture only V1-readable values into `LegacyMigrationBridge`; in `didMigrate`, match by UUID, assign `.local`, convert displayed calendar date to `LocalDate`, persist the summary, then remove the bridge only after save. `AppGroupBootstrapLock` wraps every disk container open/migration before SwiftData is available. Failure leaves source/bridge for `StoreFailureView`; never reset. `Baby.photoData` and `ActiveTimer.liveActivityID` have no protocol projection.

Implement `AppGroupFencedLease` in `BabyStore`, not `BabySync`, with a per-App-Group advisory lock file held only around lease CAS and `withFencedCommit`; `Package.swift` adds executable target `BabyStoreLeaseProbe` depending on `BabyStore` so tests can launch real independent processes. Persist a `FencedLease` row as `{scopeKey, ownerID, generation, expiresAt}`. `acquire` takes the OS lock, grants an absent/expired row by incrementing `generation`, and returns nil for a live different owner; `renew` requires exact owner/generation before replacing `expiresAt`; `withFencedCommit` takes the OS lock, rereads exact owner/generation and nonexpired time, runs the supplied no-suspension SwiftData transaction, then releases the lock; `cancel` only clears the exact owner/generation. No network/refresh/WebSocket wait runs inside `withFencedCommit`. An old process paused outside that short commit region can be overtaken after expiry, but its resumed commit sees a different generation and fails `staleLease`; it must discard its response. Run probes with a shared temporary App Group path to assert app-vs-widget realm takeover, stale commit rejection, and account-scope refresh serialization.

Define `AccountLeaseKey` in `BabyStore/Models/AccountLeaseKey.swift` using only Foundation scalar identity fields. `LeaseScope.account` accepts this neutral key from P3.1 onward; `BabyStore`, `BabyStoreLeaseProbe`, and `AccountLeaseKeyTests` must compile without importing `BabySync` or using `AccountSessionKey`. In `AccountLeaseKeyTests.swift`, cover Codable round-trip and distinct lease scopes when any one origin/instance/account/device field differs; in `AppGroupFencedLeaseTests.swift`, run account-scope serialization probes with this key. P3.3 owns the later Keychain-key adapter in `BabySync`.

Implement `AppGroupWriteBoundary.swift` as the common boundary for every app/widget/intent `BabyStore` write, including local-only writes and presentation-only timer saves. `captureWriteContext` reads active realm and transition generation under the same App Group OS advisory lock used by `AppGroupFencedLease`. `withRealmWrite(expected:_:)` acquires that lock, rereads durable active realm/generation/transition phase, rejects an exclusive migration/rewrite/snapshot/realm-switch phase or stale generation with `retryAfterTransition`, rejects referenced entities outside the expected realm with `wrongRealm`, and runs the no-suspension body inside one SwiftData transaction before releasing the lock. The boundary owns the transaction; callers do not nest transactions or reacquire the lock inside it. Import/snapshot/realm commits use the same lock through `withFencedCommit` and advance the generation before releasing it. A local writer either commits completely before a transition begins or fails/retries against the new realm after it ends; requesting sync after an unguarded save is insufficient.

Implement `AccountAccessStore.swift` with a non-secret `AccountAccessState` SchemaV2 model keyed by `AccountLeaseKey`, containing authorization state, bound realm identities, current local credential version, and durable `RefreshAttemptState` (initially `.idle`). `markAccountSignedOut` atomically denies ordinary sync and invalidates the account and bound-realm lease/transition generations under that same lock. `markAccountAuthenticated` records successful fresh authorization; a missing/denied account row fails closed. `accountAllowsOrdinarySync` rereads durable state rather than a per-process cache. Test account gate persistence/isolation and stale write-generation rejection in `AppGroupFencedLeaseTests.swift`, using `BabyStoreLeaseProbe` processes and neutral keys only. Local-only tracking requires no account row. All test suites/helpers accessing `BabyStore` or the coordinator are `@MainActor`; the snippets below execute within that isolation.

Implement the refresh-attempt methods in `AccountAccessStore.swift` using P3.1's neutral `AccountLeaseKey` and account-scoped token only. Under the same short OS lock, `beginRefreshAttempt` checks the current owner/generation, matching stored credential version and `.idle`, then durably saves `.inFlightUncertain(RefreshAttempt(...))` before any network bytes may leave. Lease expiry, cancellation, process death and `markAccountSignedOut` never clear that marker. `clearRefreshAttemptAfterProvenNoSend` accepts only the matching attempt and current fence after the caller proves transport was never invoked; a timeout, disconnect, lost reply or unknown send status stays uncertain. `finishRefreshAttempt` checks the current fence and exact persisted attempt ID before invoking its synchronous credential-install closure, then saves the new credential version and `.idle` before releasing the lock. All app/widget readers use this boundary, so no live process observes a half-install. Keychain and SwiftData are separate durable systems: a crash after Keychain installation but before clearing the marker deliberately remains uncertain and must take the fresh-proof path on restart, never reuse either stored refresh token. A stale holder cannot invoke the installer or clear/replace a successor's marker. `AppGroupFencedLeaseTests.swift` covers these store-only transitions and crash ordering with a fake install closure; no store type imports a Keychain/session DTO.

- [ ] **Step 4: Run focused GREEN and package-count tests (2–5 minutes)**


Run: `rtk swift test --package-path Packages/BabyTracker --filter 'SchemaMigrationTests|RealmIsolationTests|RealmTransitionTests|AccountLeaseKeyTests|AppGroupFencedLeaseTests|ProtocolFixtureConformanceTests|LocalDateTests' && rtk make test`

Expected: PASS; a closed/reopened, fixture-builder-generated V1 SQLite/WAL/SHM store preserves every UUID, photo bytes, timer, and legacy log while converting the displayed date under a changed time zone; protocol fixtures decode in Swift; local-only writes create no outbox row; stale selected IDs resolve only inside the active realm; and two real probe processes prove takeover, stale-owner fenced-commit denial, renewal, cancellation, and account-scope refresh serialization without relying on the in-process container lock.

- [ ] **Step 5: Commit the migration and protocol foundation (2–5 minutes)**

```bash
rtk git add Packages/BabyTracker/Package.swift Packages/BabyTracker/Sources/BabyProtocol Packages/BabyTracker/Sources/BabyCore/LocalDate.swift Packages/BabyTracker/Sources/BabyStore Packages/BabyTracker/Sources/BabyStoreLeaseProbe Packages/BabyTracker/Sources/BabyFeature/SelectedBabyStore.swift Packages/BabyTracker/Tests/BabyProtocolTests Packages/BabyTracker/Tests/BabyStoreTests Makefile
rtk git commit -m "feat: add family sync realm migration"
```

### Task 2: Transactional shared writes and receipt-safe timer intents (P3.2)

**Files:**

- Create: `Packages/BabyTracker/Sources/BabyStore/{BabyStore+Outbox,OverlayMaterializer,MutationFactory,TimerIntentReducer}.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/{BabyStore,BabyStore+Babies,BabyStore+Feeding,BabyStore+Sleep,BabyStore+Diaper,BabyStore+Timers,LiveActivityController}.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/Models/{ActiveTimer,SyncOutboxEntry,CanonicalEntityBase}.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/EntryPayload.swift`
- Create: `Packages/BabyTracker/Tests/BabyStoreTests/{OutboxTransactionTests,SharedTimerTests,OverlayMaterializerTests}.swift`
- Create: `Packages/BabyTracker/Tests/BabyStoreTests/SyncIntentTestAdapter.swift`

**Interfaces:**

- Consumes: P2 typed `WireMutation`, exact P2 timer operations (`timer_start`, `timer_switch_side`, `timer_attach_bottle`, `timer_stop`), `Outcome`, and `DeliveryState` semantics.
- Produces:

```swift
public enum MutationDeliveryState: String, Codable, Sendable {
    case neverDispatched, inFlightOrUncertain, acknowledged, quarantined, superseded
}

public struct EntityOverlay: Codable, Sendable, Equatable {
    public let entityID: UUID
    public let entityKind: WireEntityKind
    public let orderedMutationIDs: [UUID]
}

public enum TimerIntent: Sendable, Equatable {
    case start(timerID: UUID, babyID: UUID, kind: TimerKind, side: BreastSide?)
    case switchSide(timerID: UUID, side: BreastSide)
    case attachBottle(timerID: UUID, volumeML: Double, contents: BottleContents)
    case stop(timerID: UUID, completedRecordID: UUID)
}

public enum TimerIntentResult: Sendable, Equatable {
    case folded(startMutationID: UUID)
    case compacted(completionMutationID: UUID)
    case dependencyBlocked(mutationID: UUID, dependsOn: UUID)
    case ready(mutationID: UUID)
    case superseded(mutationID: UUID, code: String)
}

public struct PersistedDispatch: Sendable, Equatable {
    public let mutationID: UUID
    public let frozenPayload: Data
    public let payloadHash: Data
    public let deliveryState: MutationDeliveryState
}

@MainActor public extension BabyStore {
    func enqueueIntent(_ mutation: WireMutation, overlay: EntityOverlay) throws
    func logDiaper(_ contents: DiaperContents, for babyID: UUID, at date: Date? = nil, consistency: String? = nil, color: String? = nil, expectedWriteContext: RealmWriteContext? = nil) throws -> DiaperEntry
    func nextDispatchBatch(limit: Int) throws -> [WireMutation]
    func markDispatched(mutationIDs: [UUID]) throws
    func persistedDispatch(mutationID: UUID) throws -> PersistedDispatch
    func reduceTimerIntent(_ intent: TimerIntent) throws -> TimerIntentResult
    func resolveTimerStartReceipt(_ outcome: Outcome) throws -> TimerIntentResult
}

// Test-only adapter in BabyStoreTests/SyncIntentTestAdapter.swift, not a new product API.
@MainActor struct SyncIntentTestAdapter {
    struct StartedFeed { let timerID: UUID; let mutationID: UUID }
    struct StoppedFeed { let recordID: UUID; let mutationID: UUID }
    let store: BabyStore
    func startFeed(side: BreastSide?, for babyID: UUID) throws -> StartedFeed
    func stopFeed(timerID: UUID) throws -> StoppedFeed
}
```

- [ ] **Step 1: Write failing atomic-write and timer receipt-loss tests (2–5 minutes)**

```swift
@Test func test_uncertainStartDoesNotDispatchOrRetargetDependentStop() throws {
    let store = try BabyStore.sharedFixture()
    let intents = SyncIntentTestAdapter(store: store)
    let start = try intents.startFeed(side: .left, for: Fixture.babyID)
    try store.markDispatched(mutationIDs: [start.mutationID])
    let frozen = try store.persistedDispatch(mutationID: start.mutationID)
    let stop = try intents.stopFeed(timerID: start.timerID)
    #expect(frozen.frozenPayload == try store.outboxEntry(start.mutationID).payload)
    #expect(try store.outboxEntry(stop.mutationID).dependencyMutationID == start.mutationID)
    #expect(try store.nextDispatchBatch(limit: 10).map(\.id) == [start.mutationID])
}

@Test(arguments: ["applied", "superseded", "rejected"])
func test_alreadyAppliedOriginalStatusesReleaseOnlyAppliedStart(originalStatus: String) throws {
    // Each table row gets its own store and exactly one stable receipt.
    let store = try BabyStore.sharedFixture()
    let intents = SyncIntentTestAdapter(store: store)
    let start = try intents.startFeed(side: .left, for: Fixture.babyID)
    try store.markDispatched(mutationIDs: [start.mutationID])
    #expect(try store.persistedDispatch(mutationID: start.mutationID).deliveryState == .inFlightOrUncertain)
    let dependent = try intents.stopFeed(timerID: start.timerID)
    #expect(try store.outboxEntry(dependent.mutationID).dependencyMutationID == start.mutationID)
    #expect(try store.nextDispatchBatch(limit: 10).map(\.id) == [start.mutationID])
    let result = try store.resolveTimerStartReceipt(Fixture.alreadyApplied(start.mutationID, originalStatus: originalStatus))
    switch originalStatus {
    case "applied":
        #expect(result == .ready(mutationID: dependent.mutationID))
        #expect(try store.nextDispatchBatch(limit: 10).map(\.id) == [dependent.mutationID])
    case "superseded", "rejected":
        let code = originalStatus == "superseded" ? "timer_collision" : "timer_start_rejected"
        #expect(result == .superseded(mutationID: dependent.mutationID, code: code))
        #expect(try store.persistedDispatch(mutationID: dependent.mutationID).deliveryState == .superseded)
        #expect(try store.nextDispatchBatch(limit: 10).isEmpty)
    default: Issue.record("Unexpected original receipt status in table")
    }
}

@Test func test_whollyNeverDispatchedTimerCompactsToOneCompletion() throws {
    let store = try BabyStore.sharedFixture()
    let intents = SyncIntentTestAdapter(store: store)
    let start = try intents.startFeed(side: .left, for: Fixture.babyID)
    _ = try intents.stopFeed(timerID: start.timerID)
    #expect(try store.nextDispatchBatch(limit: 10).map(\.operation) == [.createFeed])
}
```

- [ ] **Step 2: Run the focused RED tests and verify old direct timer saves cannot meet them (2–5 minutes)**

Run: `rtk swift test --package-path Packages/BabyTracker --filter 'OutboxTransactionTests|SharedTimerTests|OverlayMaterializerTests'`

Expected: FAIL because shared writes still use `ChangeLogEntry`/direct saves and no frozen delivery state or dependent-command reducer exists.

- [ ] **Step 3: Implement one SwiftData transaction for entity, base/overlay, and outbox changes (2–5 minutes)**

```swift
func markDispatched(mutationIDs: [UUID]) throws {
    let expected = try captureWriteContext()
    try withRealmWrite(expected: expected) {
        for id in mutationIDs {
            let entry = try requiredOutboxEntry(id)
            guard entry.deliveryState == .neverDispatched else { continue }
            entry.freezePayloadHashIfNeeded()
            entry.deliveryState = .inFlightOrUncertain
        }
    }
}

func reduceTimerIntent(_ intent: TimerIntent) throws -> TimerIntentResult {
    switch (intent, try timerDeliveryState(intent.timerID)) {
    case (.switchSide, .neverDispatched), (.attachBottle, .neverDispatched): return try foldIntoUndispatchedStart(intent)
    case (.stop, .neverDispatched): return try compactUndispatchedLifecycle(intent)
    case (.switchSide, .inFlightOrUncertain), (.attachBottle, .inFlightOrUncertain), (.stop, .inFlightOrUncertain): return try appendDependencyBlockedIntent(intent)
    default: return try appendAcknowledgedTimerCommand(intent)
    }
}

func resolveTimerStartReceipt(_ outcome: Outcome) throws -> TimerIntentResult {
    let original = outcome.status == .alreadyApplied ? try outcome.requiredOriginalStatus : outcome.status
    switch original {
    case .applied: return try releaseDependents(of: outcome.mutationID)
    case .superseded: return try supersedeDependents(of: outcome.mutationID, code: "timer_collision")
    case .rejected: return try supersedeDependents(of: outcome.mutationID, code: "timer_start_rejected")
    default: throw SyncPersistenceError.unstableTimerReceipt
    }
}
```

Make all shared `BabyStore` intent-named writes atomically update their materialized entity and exactly one outbox row. Keep local-only writes immediate with no outbox. `PersistedDispatch` is the persisted-state read after the `Void` `markDispatched` command; it is the only assertion source for frozen bytes/hash/state. Presentation-only `liveActivityID` writes remain local and do not create protocol mutations. Never edit an `inFlightOrUncertain` payload/ID; only `applied`, including `already_applied` with `original_status: applied`, releases dependent commands. `already_applied` with original `superseded` or `rejected` marks the local dependents terminally superseded and never retargets another caregiver's timer.

Keep the receipt-status table in `SharedTimerTests.swift`. Its `Fixture.alreadyApplied(_:originalStatus:)` helper accepts one of the three raw wire status strings and decodes the corresponding P2 stable `already_applied` receipt with the fixture mutation ID. Each invocation creates a fresh store, durably marks the start dispatched before appending a persisted dependent stop, and applies exactly one receipt. Receipt resolution atomically terminalizes that start and either releases or supersedes its dependents; the dispatch-batch assertions prove only the applied case releases the stop. Do not apply mutually exclusive stable receipts to one start, or use the wholly never-dispatched start/stop lifecycle that intentionally compacts.

`SyncIntentTestAdapter.startFeed` delegates to the existing `BabyStore.startFeed(side:for:) -> ActiveTimer`; `stopFeed` finds that persisted timer in the fixture store and calls existing `BabyStore.stopFeed(_:) -> FeedEntry`. The adapter returns the actual generated timer/record UUID and looks up the corresponding persisted outbox mutation ID by entity/operation, requiring a unique matching row. It must never infer mutation IDs from domain IDs, create an outbox row, or manufacture a receipt. Its wholly-undispatched stop reads the persisted compacted completion mutation. P3.4 extends this same test-only adapter to read the matching durable post-import intent when the manifest is frozen; production timer APIs keep their existing return types.

Route `BabyStore+Babies`, feeding/sleep/diaper/timer writes, reducer operations, and `setLiveActivityID` through P3.1's `captureWriteContext`/`withRealmWrite` before any entity or outbox save. Add the optional `expectedWriteContext: RealmWriteContext? = nil` argument shown for the existing `logDiaper` API to each user-write entry point: nil captures at entry, while app/widget/intent callers editing a previously rendered realm supply their captured context. Existing return types and call sites remain source-compatible. Split internal mutation helpers from public boundaries so one user operation takes the OS lock and transaction exactly once; callbacks/widget reloads run after release. Preserve the caller's expected realm/generation across an edit, and verify each referenced entity's realm inside the boundary. Extend `OutboxTransactionTests.swift` with stale-generation rejection leaving both domain rows and outbox unchanged; P3.4 proves the real widget-save race with the process probe.

- [ ] **Step 4: Run focused GREEN and full package suite (2–5 minutes)**

Run: `rtk swift test --package-path Packages/BabyTracker --filter 'OutboxTransactionTests|SharedTimerTests|OverlayMaterializerTests' && rtk make test`

Expected: PASS; commit rollback leaves neither entity nor outbox partial, pre-dispatch side/bottle changes fold, offline start/stop compacts, `persistedDispatch` survives reload with unchanged bytes/hash, uncertain dependencies wait for the exact receipt, all three `already_applied.original_status` cases are terminally distinct, and all legacy local timer tests still pass.

- [ ] **Step 5: Commit shared-write and timer intent semantics (2–5 minutes)**

```bash
rtk git add Packages/BabyTracker/Sources/BabyStore Packages/BabyTracker/Tests/BabyStoreTests Makefile
rtk git commit -m "feat: persist family sync outbox intents"
```

### Task 3: Endpoint confirmation, Keychain sessions, Apple sign-in, and family settings (P3.3)

**Files:**

- Create: `Packages/BabyTracker/Sources/BabySync/{ServerOrigin,EndpointIdentity,ServerSelection,KeychainSessionStore,AccountSessionKey+Lease,AppleNonce,AppleSignInClient,AppleCredentialStateClient,AccountCoordinator,FamilyClient,InvitationParser,InvitationQRCode}.swift`
- Create: `Packages/BabyTracker/Sources/SettingsFeature/{FamilySyncSettingsModel,FamilySettingsViewModel,FamilySettingsView,ServerSelectionView,InvitationScannerView,InvitationQRCodeView,AccountSettingsView,DisplayNameConfirmationView,FamilyLifecycleConfirmationView}.swift`
- Modify: `Packages/BabyTracker/Sources/SettingsFeature/{SettingsView,SettingsViewModel}.swift`
- Modify: `Apps/BabyTrackerApp/{AppEnvironment,RootView,BabyTrackerApp}.swift`
- Modify: `Apps/BabyTracker.entitlements`, `BabyTracker.xcodeproj/project.pbxproj`
- Create: `Packages/BabyTracker/Tests/BabySyncTests/{ServerOriginTests,EndpointIdentityTests,AppleNonceTests,KeychainSessionStoreTests,AccountSessionLeaseTests,DeviceContinuityTests,AppleCredentialStateTests,InvitationParserTests,InvitationQRCodeTests}.swift`
- Create: `Packages/BabyTracker/Tests/SettingsFeatureTests/{FamilySettingsViewModelTests,ServerSelectionTests,FamilyLifecycleViewModelTests,DisplayNameConfirmationViewModelTests}.swift`
- Create: `UITests/BabyTrackerUITests/FamilySyncOnboardingUITests.swift`

**Interfaces:**

- Consumes: P1 `ServerInfo`, challenge/exchange/session/family/invitation/lifecycle OpenAPI operations and the P1 D6 nonce fixture; `GET /v1/server` remains the identity probe.
- Consumes: P3.1's `BabyStore.AccountLeaseKey`; `AccountSessionKey` stays owned by `BabySync/KeychainSessionStore.swift`, and `BabySync/AccountSessionKey+Lease.swift` provides the one-way identity adapter below.
- Produces:

```swift
public protocol SecureSessionStore: Sendable {
    func loadAccount(_ key: AccountSessionKey) throws -> SessionCredentials?
    func saveAccount(_ credentials: SessionCredentials, for key: AccountSessionKey) throws
    func rekeyAccount(_ oldKey: AccountSessionKey, to newKey: AccountSessionKey, checkpointID: UUID) throws
    func loadDeviceContinuity(for key: ContinuityKey) throws -> DeviceContinuityBinding?
    func rekeyFamily(_ oldKey: FamilySessionKey, to newKey: FamilySessionKey, checkpointID: UUID) throws
    func promote(_ accountKey: AccountSessionKey, to familyKey: FamilySessionKey, checkpointID: UUID) throws
    func resumePromotion(checkpointID: UUID) throws -> FamilySessionKey?
    func loadFamily(_ key: FamilySessionKey) throws -> SessionCredentials?
    func saveFamily(_ credentials: SessionCredentials, for key: FamilySessionKey) throws
    func deleteFamily(_ key: FamilySessionKey) throws
    func invalidateSessionsRetainingContinuity(for accountKey: AccountSessionKey) throws
}

public struct AccountSessionKey: Codable, Sendable, Hashable { public let canonicalOrigin: String; public let serverInstanceID: UUID; public let accountID: UUID; public let deviceID: UUID; public let recoveryEpoch: UInt64 }
public extension AccountSessionKey {
    var leaseKey: AccountLeaseKey {
        AccountLeaseKey(canonicalOrigin: canonicalOrigin, serverInstanceID: serverInstanceID, accountID: accountID, deviceID: deviceID)
    }
}
public struct FamilySessionKey: Codable, Sendable, Hashable { public let canonicalOrigin: String; public let serverInstanceID: UUID; public let familyID: UUID; public let accountID: UUID; public let deviceID: UUID; public let recoveryEpoch: UInt64 }
public struct ContinuityKey: Codable, Sendable, Hashable { public let canonicalOrigin: String; public let serverInstanceID: UUID; public let accountID: UUID; public let deviceID: UUID }
public struct DeviceContinuity: Codable, Sendable, Equatable { public let deviceID: UUID; public let credential: String }
public struct DeviceContinuityBinding: Codable, Sendable, Equatable { public let accountID: UUID; public let continuity: DeviceContinuity }
public struct SessionCredentials: Sendable, Equatable { public let accessToken: String; public let refreshToken: String; public let expiresAt: Date; public let recoveryEpoch: UInt64; public let deviceContinuity: DeviceContinuity; public let credentialVersion: UUID }
public enum AppleCredentialState: Sendable, Equatable { case authorized, revoked, notFound, transferred }
public enum FamilySyncMode: Sendable, Equatable { case localOnly, endpointConfirmed(ConfirmedEndpoint), authenticated(AccountSessionKey), shared(FamilySessionKey), recovery(FamilySessionKey), signedOut(AccountSessionKey) }
public struct InvitationIntent: Sendable, Equatable { public let origin: ServerOrigin; public let token: String }
public struct FamilyInvitation: Sendable, Equatable { public let id: UUID; public let shareURL: URL }
public struct InvitationQRCode: Sendable, Equatable { public let invitationID: UUID; public let payload: String; public let imageData: Data }
public struct FamilyMembership: Sendable, Equatable { public let familyID: UUID; public let accountID: UUID; public let deviceID: UUID; public let recoveryEpoch: UInt64 }
public struct CaregiverProfile: Sendable, Equatable { public let accountID: UUID; public let displayName: String; public let role: String }
public struct FamilyMember: Sendable, Equatable, Identifiable { public let id: UUID; public let displayName: String; public let role: String }
public enum FamilyLifecycleCommand: Sendable, Equatable { case removeMember(UUID), transferOwnership(UUID), deleteAccount }
public enum FamilySyncError: Error, Sendable, Equatable { case endpointConfirmationRequired, deviceContinuityInvalid, deviceContinuityRequired, displayNameConfirmationRequired, ownerTransferRequired, ownerCredentialStateRequired }

public struct ServerOrigin: Sendable, Hashable { public let canonicalString: String; public init(validating input: String) throws }
public struct ConfirmedEndpoint: Sendable, Hashable { public let origin: ServerOrigin; public let serverInstanceID: UUID }
public func appleNonce(canonicalOrigin: String, serverInstanceID: UUID, challenge: String) -> String

@MainActor public protocol FamilySyncSettingsModel: AnyObject {
    var mode: FamilySyncMode { get }
    var profile: CaregiverProfile { get }
    var members: [FamilyMember] { get }
    func confirmEndpoint(_ endpoint: ConfirmedEndpoint) async throws
    func signInWithApple() async throws
    func refreshAppleCredentialState() async throws -> AppleCredentialState
    func confirmDisplayName(_ name: String) async throws
    func updateDisplayName(_ name: String) async throws
    func createFamily(name: String) async throws
    func disconnectCurrentFamily() async throws
    func acceptInvitation(_ invitation: InvitationIntent) async throws
    func createInvitation() async throws -> FamilyInvitation
    func invitationShareURL(_ invitation: FamilyInvitation) throws -> URL
    func invitationQRCode(_ invitation: FamilyInvitation) throws -> InvitationQRCode
    func revokeInvitation(_ invitationID: UUID) async throws
    func leaveFamily() async throws
    func removeMember(_ memberID: UUID) async throws
    func transferOwnership(to memberID: UUID) async throws
    func scheduleFamilyDeletion() async throws
    func recoverFamily() async throws
    func forfeitFamilyRecovery() async throws
    func deleteAccount() async throws
}
```

- [ ] **Step 1: Write failing endpoint/nonce/Keychain and UI confirmation tests (2–5 minutes)**

```swift
@Test func test_nonceVectorUsesNULSeparatorsAndSelectedOrigin() throws {
    let vector = try ProtocolFixture.authNonce(named: "nonce-nul-origin-instance-v1")
    #expect(appleNonce(canonicalOrigin: vector.canonicalOrigin, serverInstanceID: vector.serverInstanceID, challenge: vector.challenge) == vector.sha256)
    #expect(appleNonce(canonicalOrigin: "https://relay.example", serverInstanceID: vector.serverInstanceID, challenge: vector.challenge) != vector.sha256)
}

@Test func test_accountSessionAdaptsToStoreOwnedLeaseIdentity() throws {
    let fixture = try AccountSessionFixture.firstAppleSignIn()
    let key = fixture.initial.accountKey
    #expect(key.leaseKey == AccountLeaseKey(canonicalOrigin: key.canonicalOrigin, serverInstanceID: key.serverInstanceID, accountID: key.accountID, deviceID: key.deviceID))
    #expect(LeaseScope.account(key.leaseKey) == .account(key.leaseKey))
}

@Test func test_disconnectPurgesOnlySharedRealmAndExplainsNoLocalRestore() async throws {
    let model = try FamilySettingsFixture.modelWithLocalAndSharedRows()
    try await model.disconnectCurrentFamily()
    #expect(try model.store.babies(in: .local).map(\.id) == [Fixture.localBabyID])
    #expect(try model.store.babies(in: Fixture.sharedRealm).isEmpty)
    #expect(model.confirmationCopy.contains("does not restore a local copy"))
}

@Test func test_deviceContinuitySurvivesFreshAppleProofAndRejectsCrossAccountClaim() async throws {
    let fixture = try AccountSessionFixture.firstAppleSignIn()
    try fixture.keychain.saveAccount(fixture.initial.credentials, for: fixture.initial.accountKey)
    let relaunched = try fixture.relaunch()
    let request = try relaunched.coordinator.freshAppleExchangeRequest()
    #expect(request.deviceContinuity == fixture.initial.credentials.deviceContinuity)
    #expect(try await fixture.server.exchangeApple(request).deviceID == fixture.initial.accountKey.deviceID)
    await #expect(throws: FamilySyncError.deviceContinuityInvalid) { try await fixture.crossAccountServer.exchangeApple(request) }
}

@Test func test_keychainPromotionResumesAfterSecItemUpdateBeforeRealmBinding() throws {
    let fixture = try AccountSessionFixture.firstAppleSignIn()
    try fixture.keychain.saveAccount(fixture.initial.credentials, for: fixture.initial.accountKey)
    try fixture.transitionStore.saveCredentialPromotionCheckpoint(accountKey: fixture.initial.accountKey, familyKey: fixture.familyKey)
    try fixture.keychain.promote(fixture.initial.accountKey, to: fixture.familyKey, checkpointID: fixture.transitionStore.promotionCheckpointID)
    #expect(try fixture.transitionStore.activeRealm == .local)
    #expect(try fixture.relaunch().keychain.resumePromotion(checkpointID: fixture.transitionStore.promotionCheckpointID) == fixture.familyKey)
}

@Test func test_profileAndLifecycleMethodsRequireConfirmedStates() async throws {
    let model = try FamilySettingsFixture.ownerModelWithMissingAppleName()
    await #expect(throws: FamilySyncError.displayNameConfirmationRequired) { try await model.createFamily(name: "Family") }
    try await model.confirmDisplayName("Alex")
    try await model.createFamily(name: "Family")
    await #expect(throws: FamilySyncError.ownerTransferRequired) { try await model.deleteAccount() }
}

@Test func test_scheduledFamilyRecoveryRestoresOnlyOwnerAndConsumesClaim() async throws {
    let model = try FamilySettingsFixture.ownerModelWithTwoMembers()
    try await model.scheduleFamilyDeletion()
    try await model.signInWithApple() // P1 requires fresh proof for the recovery-only claim.
    guard case .recovery = model.mode else { Issue.record("Expected recovery-only claim"); return }
    try await model.recoverFamily()
    guard case .shared = model.mode else { Issue.record("Expected recovered shared family"); return }
    #expect(model.members.count == 1)
    #expect(model.members.first?.role == "owner")
    await #expect(throws: FamilySyncError.ownerTransferRequired) { try await model.deleteAccount() }
}

@Test func test_scheduledFamilyForfeitReleasesSlotWithoutRecovering() async throws {
    let model = try FamilySettingsFixture.ownerModelWithTwoMembers()
    try await model.scheduleFamilyDeletion()
    try await model.signInWithApple()
    guard case .recovery = model.mode else { Issue.record("Expected recovery-only claim"); return }
    try await model.forfeitFamilyRecovery()
    guard case .authenticated = model.mode else { Issue.record("Expected account without family slot"); return }
    #expect(try model.store.babies(in: Fixture.sharedRealm).isEmpty)
    try await model.deleteAccount() // No active owner membership or recovery claim remains.
}

@Test func test_profileEditPersists() async throws {
    let model = try FamilySettingsFixture.ownerModel()
    try await model.updateDisplayName("Taylor")
    #expect(model.profile.displayName == "Taylor")
}

@Test(arguments: [AppleCredentialState.revoked, .notFound])
func test_credentialStateLossSignsOutAndDeniesOrdinarySync(state: AppleCredentialState) async throws {
    let fixture = try FamilySettingsFixture.selfHostedCaregiverAndOtherAccount()
    fixture.model.appleCredentialStateClient.nextState = state
    #expect(try await fixture.model.refreshAppleCredentialState() == state)
    #expect(fixture.model.mode == .signedOut(fixture.accountKey))
    #expect(try fixture.keychain.loadFamily(fixture.familyKey) == nil)
    #expect(try fixture.model.store.babies(in: fixture.sharedRealm).isEmpty)
    await fixture.requestOrdinarySyncFromAppAndExtension()
    #expect(fixture.transport.ordinarySyncRequestCount == 0)
    #expect(fixture.transport.logoutRequestCount == 0)
    #expect(fixture.transport.requestsUsingErasedSessionCount == 0)
    #expect(try fixture.keychain.loadDeviceContinuity(for: fixture.continuityKey) == fixture.originalContinuityBinding)
    #expect(try fixture.keychain.loadFamily(fixture.otherAccountFamilyKey) == fixture.otherAccountCredentials)
    #expect(fixture.server.familyDataAndOtherMembershipsUnchanged)
    #expect(fixture.model.confirmationCopy.contains("Apple account settings"))
}

@Test func test_signedOutSelfHostRequiresFreshAppleProofBeforeResumingSync() async throws {
    let fixture = try FamilySettingsFixture.signedOutSelfHostedCaregiver()
    await fixture.requestOrdinarySyncFromAppAndExtension()
    #expect(fixture.transport.ordinarySyncRequestCount == 0)
    try await fixture.model.signInWithApple()
    #expect(fixture.appleClient.freshProofRequestCount == 1)
    #expect(fixture.server.lastExchangeUsedBoundDeviceContinuity)
    #expect(fixture.server.lastExchangeDeviceID == fixture.originalDeviceID)
    #expect(fixture.transport.logoutRequestCount == 0)
    await fixture.requestOrdinarySyncFromAppAndExtension()
    #expect(fixture.transport.ordinarySyncRequestCount == 1)
}

@Test func test_ownerMemberControlsAndAccountDeletionPreconditionUseP1LifecycleCommands() async throws {
    let model = try FamilySettingsFixture.ownerModelWithTwoMembers()
    try await model.removeMember(Fixture.removableMemberID)
    try await model.transferOwnership(to: Fixture.successorMemberID)
    try await model.deleteAccount()
    #expect(model.lastLifecycleCommands == [.removeMember(Fixture.removableMemberID), .transferOwnership(Fixture.successorMemberID), .deleteAccount])
}

@Test func test_qrUsesTheExactSameInvitationLinkAndTokenAsShare() async throws {
    let model = try FamilySettingsFixture.ownerModel()
    let qr = try await model.createInvitationQRCode()
    #expect(qr.payload == try await model.shareInvitationLink())
}

@Test func test_accountDeletionRevokesFamilySessionAndDeviceContinuityBinding() async throws {
    let fixture = try AccountSessionFixture.familySession()
    try await fixture.model.deleteAccount()
    #expect(try fixture.keychain.loadFamily(fixture.familyKey) == nil)
    #expect(try fixture.keychain.loadDeviceContinuity(for: fixture.continuityKey) == nil)
}
```

- [ ] **Step 2: Run focused RED tests and confirm no current target provides these boundaries (2–5 minutes)**

Run: `rtk swift test --package-path Packages/BabyTracker --filter 'ServerOriginTests|EndpointIdentityTests|AppleNonceTests|KeychainSessionStoreTests|AccountSessionLeaseTests|DeviceContinuityTests|AppleCredentialStateTests|InvitationParserTests|InvitationQRCodeTests|FamilySettingsViewModelTests|FamilyLifecycleViewModelTests|DisplayNameConfirmationViewModelTests|ServerSelectionTests'`

Expected: FAIL because `BabySync` has only the legacy `PendingChange` seam and Settings has no endpoint/session model.

- [ ] **Step 3: Implement confirmation-first endpoint/auth composition and Settings injection (2–5 minutes)**

```swift
public func appleNonce(canonicalOrigin: String, serverInstanceID: UUID, challenge: String) -> String {
    var bytes = Data("baby-tracker-auth-v1".utf8)
    bytes.append(0)
    bytes.append(contentsOf: canonicalOrigin.utf8)
    bytes.append(0)
    bytes.append(contentsOf: serverInstanceID.uuidString.lowercased().utf8)
    bytes.append(0)
    bytes.append(contentsOf: challenge.utf8)
    return SHA256.hash(data: bytes).compactMap { String(format: "%02x", $0) }.joined()
}

func acceptInvitation(_ invitation: InvitationIntent) async throws {
    guard invitation.origin == confirmedEndpoint.origin else { throw FamilySyncError.endpointConfirmationRequired }
    try await authenticatedFamilyClient.confirmJoin(token: invitation.token)
}

func promoteAfterMembership(_ membership: FamilyMembership) throws {
    let accountKey = AccountSessionKey(canonicalOrigin: confirmedEndpoint.origin.canonicalString, serverInstanceID: confirmedEndpoint.serverInstanceID, accountID: membership.accountID, deviceID: membership.deviceID, recoveryEpoch: membership.recoveryEpoch)
    let familyKey = FamilySessionKey(canonicalOrigin: accountKey.canonicalOrigin, serverInstanceID: accountKey.serverInstanceID, familyID: membership.familyID, accountID: accountKey.accountID, deviceID: accountKey.deviceID, recoveryEpoch: membership.recoveryEpoch)
    try transitionStore.saveCredentialPromotionCheckpoint(accountKey: accountKey, familyKey: familyKey)
    try keychain.promote(accountKey, to: familyKey, checkpointID: transitionStore.promotionCheckpointID)
    try transitionStore.markMembershipReady(familyKey: familyKey)
}
```

`ServerOrigin` accepts an HTTPS origin only and rejects credentials, paths, queries, fragments, and redirects. Parsing produces only an `InvitationIntent`; probe `GET /v1/server`, bind the confirmed origin to its stable instance ID, block a changed known instance until explicit confirmation, and never accept an advertised origin as replacement. The first P1 exchange is saved under `AccountSessionKey`; its returned one-time `device_credential` is retained under the exact `ContinuityKey`. Every fresh Apple exchange loads only that account-qualified binding. `promote` performs one Keychain update and records `membershipReady`; it does **not** bind or expose a shared realm. P3.4 performs the sole realm activation after mapping/rewrite/canonical-baby establishment. Account/device deletion removes continuity. The app target checks Apple credential state on launch/foreground using the separately stored Apple user binding. After Apple returns a name, seed editable display name; when absent, require saved-name confirmation or explicit entry, never email/subject derivation. Add Sign in with Apple capability, hosted-only Associated Domain, custom scheme, and no production TLS bypass. Scanner/paste/link parsing never redeems membership. One `FamilyInvitation` produces both link and QR locally. Implement and test create/join, profile edit, leave/removal/transfer, account/family lifecycle, and self-host manual Apple-revocation guidance. Local-only instantiates no auth client. D10 lifecycle operations use the durable cleanup checkpoint described below.

Implement the account lease adapter only in `BabySync/AccountSessionKey+Lease.swift`, importing `BabyStore`; test its exact four-field projection in `AccountSessionLeaseTests.swift`. Every account refresh lease uses `.account(accountSessionKey.leaseKey)`. Neither the store schema nor its process probe imports Keychain session types.

`SessionCredentials.credentialVersion` is a client-generated non-secret UUID stored with the Keychain item, not a P1 wire field or token digest. Initial sign-in calls `markAccountAuthenticated(accountKey.leaseKey, credentialVersion: credentials.credentialVersion)` to record its version in the account access row. Each successful refresh/fresh-proof install generates a new version once and commits it through P3.1's fenced install boundary; account-to-family promotion preserves the version.

The `transitionStore` in the promotion snippets is the `BabySync` adapter owned by `AccountCoordinator.swift`: it accepts session keys there, then passes only neutral origin/instance/account/device/family/epoch fields and `RealmID` into `BabyStore`'s `RealmTransitionCheckpoint`. `BabyStore` never takes an `AccountSessionKey` or `FamilySessionKey` parameter or imports `BabySync` to persist a promotion.

Implement credential-state loss in `AccountCoordinator.swift` and `FamilySettingsViewModel.swift`, with fixtures/assertions in `AppleCredentialStateTests.swift` and `FamilySettingsViewModelTests.swift` and rendered self-host coverage in `FamilySyncOnboardingUITests.swift`. On `.revoked` or `.notFound`, first call P3.1's `markAccountSignedOut(accountKey.leaseKey)` to durably deny sync and invalidate its lease/transition generation so in-flight completions and extensions cannot commit; stop its socket/sync, send no `/logout` or other request with its old access/refresh token, and call `invalidateSessionsRetainingContinuity(for:)`. This Keychain operation makes `loadAccount`/`loadFamily` return nil, erases access/refresh material, and retains only the existing origin/instance/account-bound device proof in a continuity-only Keychain item; never expose that item as `SessionCredentials`. Purge that account's shared rows/base/outbox/cursor, end its activities and reload widgets, retain the permanent local realm, and display `.signedOut(accountKey)`. This is a local privacy policy, not server family/account deletion. P1 `/logout` revokes the device binding and therefore must not run for credential-state loss. Hosted Apple server notifications provide server-side session revocation; self-host stops locally and explains manual Apple-account Settings revocation. Explicit disconnect/account deletion retains its separate logout/device-binding-revocation flow. Exact-key targeting leaves another account's credentials and server family data/memberships untouched. P3.5/P3.6 consult `accountAllowsOrdinarySync` before every app/extension request and commit, including refresh. Only successful fresh Apple proof with continuity and revalidated membership calls `markAccountAuthenticated` and starts a fresh authorized snapshot; cached self-host session TTL is never an exception. If no continuity proof remains, surface P1's required/invalid error rather than silently replacing a device. Mirror revoked and notFound for both owner and caregiver fixtures; the table above pins the caregiver/self-host regression specifically.

In `FamilyLifecycleViewModelTests.swift`, use separate fresh server/model fixtures for schedule-delete → fresh Apple proof → recover and schedule-delete → fresh Apple proof → forfeit. Scheduling revokes family sessions, purges shared cached rows under D10, and preserves only the owner's recovery slot; fresh proof establishes P1's recovery-only session. Recovery consumes the claim, restores only the owner's membership, and makes active-owner account deletion require transfer again. Forfeiture consumes the claim and slot while deletion stays scheduled, transitions to account-only state, and permits account deletion. Assert these server fixture states as well as rendered controls; never forfeit a claim already consumed by recovery. Mirror both independent branches in `FamilySyncOnboardingUITests.swift`.

- [ ] **Step 4: Run focused GREEN, UI, and full package verification (2–5 minutes)**

Run: `rtk swift test --package-path Packages/BabyTracker --filter 'ServerOriginTests|EndpointIdentityTests|AppleNonceTests|KeychainSessionStoreTests|AccountSessionLeaseTests|DeviceContinuityTests|AppleCredentialStateTests|InvitationParserTests|InvitationQRCodeTests|FamilySettingsViewModelTests|FamilyLifecycleViewModelTests|DisplayNameConfirmationViewModelTests|ServerSelectionTests' && rtk make test && rtk xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -only-testing:BabyTrackerUITests/FamilySyncOnboardingUITests`

Expected: PASS; invalid origins and redirects cannot leak credentials; the exact NUL-vector passes; account-to-family rekey resumes without duplicate secrets; device continuity survives restart/fresh Apple proof and rejects a cross-account claim; revoked/notFound credential state immediately signs out that account and blocks app/extension sync, with fresh-proof self-host recovery and other-account isolation; missing-name confirmation/profile edit, owner/member/delete/recover/forfeit, QR/share identity, custom endpoint/invitation confirmations, self-host revocation copy, and local-only no-auth behavior are rendered; no secret is a launch argument; and simulated Keychain isolation covers origin/instance/family/account/device.

- [ ] **Step 5: Commit endpoint, credential, and Settings onboarding work (2–5 minutes)**

```bash
rtk git add Packages/BabyTracker/Sources/BabySync Packages/BabyTracker/Sources/SettingsFeature Apps/BabyTrackerApp Apps/BabyTracker.entitlements BabyTracker.xcodeproj/project.pbxproj Packages/BabyTracker/Tests/BabySyncTests Packages/BabyTracker/Tests/SettingsFeatureTests UITests/BabyTrackerUITests Makefile
rtk git commit -m "feat: add confirmed family sync onboarding"
```

### Task 4: Resumable resolver-backed import and atomic realm move (P3.4)

**Files:**

- Create: `Packages/BabyTracker/Sources/BabySync/ImportCoordinator.swift`
- Create: `Packages/BabyTracker/Sources/BabyStore/{ImportStore,BabyStore+Import,ImportManifestSnapshot}.swift`
- Create: `Packages/BabyTracker/Sources/SettingsFeature/{ImportSummaryView,ImportConflictView}.swift`
- Modify: `Packages/BabyTracker/Sources/BabySync/{AccountCoordinator,FamilyClient,ServerSelection}.swift`
- Modify: `Packages/BabyTracker/Sources/SettingsFeature/{FamilySettingsViewModel,FamilySettingsView}.swift`
- Create: `Packages/BabyTracker/Tests/BabyStoreTests/ImportStoreTests.swift`
- Modify: `Packages/BabyTracker/Tests/BabyStoreTests/SyncIntentTestAdapter.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStoreLeaseProbe/main.swift`
- Create: `Packages/BabyTracker/Tests/BabySyncTests/ImportCoordinatorTests.swift`
- Create: `Packages/BabyTracker/Tests/SettingsFeatureTests/ImportConflictViewModelTests.swift`
- Create: `UITests/BabyTrackerUITests/FamilyImportUITests.swift`

**Interfaces:**

- Consumes: P2 `ImportResolveRequest`, `ImportResolverResponse`, `import_mode`, complete stable injective baby-ID map, resolver revision, explicit owner conflict `{choice: use_existing|keep_local, canonical_baby_id: UUID?}`, `ambiguous_local_babies`, record manifest, and canonical equal/different UUID outcomes.
- Produces:

```swift
public enum ImportPhase: String, Codable, Sendable {
    case endpointConfirmed, membershipReady, manifestFrozen, resolverRequested, mappingDurable, referencesRewritten, sharedRealmExposed, draining, complete, conflict
}

public struct FrozenImportManifest: Codable, Sendable, Equatable {
    public let sourceRevision: Int64
    public let chunks: [FrozenImportManifestChunk]
    public let sourceBases: [EntityKey: Data]
}

public struct EntityKey: Codable, Sendable, Hashable { public let kind: String; public let id: UUID }

public struct FrozenImportManifestChunk: Codable, Sendable, Equatable {
    public let ordinal: Int
    public let manifestPayload: Data
    public let manifestHash: Data
}

public enum ImportMode: String, Codable, Sendable { case preserveLocalIDs = "preserve_local_ids", matchShared = "match_shared" }
public enum OwnerConflictChoice: String, Codable, Sendable { case useExisting = "use_existing", keepLocal = "keep_local" }
public struct ImportConflictResolution: Codable, Sendable, Equatable {
    public let choice: OwnerConflictChoice
    public let canonicalBabyID: UUID?
    public var isValid: Bool { (choice == .useExisting && canonicalBabyID != nil) || (choice == .keepLocal && canonicalBabyID == nil) }
}
public enum ImportConflictError: Error, Sendable, Equatable { case ambiguousLocalBabies }
public enum ImportStoreError: Error, Sendable, Equatable { case nonInjectiveMapping, importModeChanged }
public enum ImportStoreValidator { public static func validate(_ response: ImportResolverResponse) throws }

@MainActor public protocol ImportStore {
    func freezeImportManifest(checkpointID: UUID) throws -> FrozenImportManifest
    func appendPostImportIntent(_ mutation: WireMutation, checkpointID: UUID) throws
    func saveMapping(_ mapping: ImportResolverResponse, checkpointID: UUID) throws
    func rewriteAndMoveWorkspace(checkpointID: UUID, to realm: RealmID) throws
    func resumeImport(checkpointID: UUID) throws -> ImportCheckpoint
}

public actor ImportCoordinator {
    public func startOrResume(checkpointID: UUID) async throws
}
```

- [ ] **Step 1: Write failing interruption, exact-match, and lease-contention tests (2–5 minutes)**

```swift
@Test func test_importRewriteLeaseExcludesWidgetWriter() throws {
    let fixture = try ImportFixture.overlappingLocalWorkspace()
    try fixture.store.saveMapping(fixture.resolverResponse, checkpointID: fixture.checkpointID)
    let widget = try fixture.launchWidgetProcess(action: .captureWriteContextThenPause)
    let rewrite = try fixture.launchAppProcess(action: .rewriteAndPauseBeforeFencedCommit)
    try widget.resume(action: .logDiaperThroughBabyStoreUsingCapturedContext)
    try rewrite.resume()
    try widget.waitForExit()
    try rewrite.waitForExit()
    #expect(try fixture.store.visibleRowsAreEntirely(in: fixture.realm))
    #expect(widget.result == .retryAfterTransition)
    #expect(try fixture.store.countDiapers(id: fixture.widgetDiaperID, acrossAllRealms: true) == 0)
    #expect(try fixture.store.countOutboxRows(entityID: fixture.widgetDiaperID, acrossAllRealms: true) == 0)
    let retried = try fixture.runWidgetProcess(action: .recaptureContextAndLogDiaper)
    #expect(retried.result == .committed)
    #expect(try fixture.store.diaper(fixture.widgetDiaperID).realmID == fixture.realm)
    #expect(try fixture.store.countOutboxRows(entityID: fixture.widgetDiaperID, acrossAllRealms: true) == 1)
}

@Test func test_mappingDurableBeforeDependentIDsChange() throws {
    let store = try ImportFixture.store()
    try store.saveMapping(ImportFixture.matchingBabyMap, checkpointID: ImportFixture.checkpointID)
    #expect(try store.feed(Fixture.feedID).babyID == Fixture.localBabyID)
    try store.rewriteAndMoveWorkspace(checkpointID: ImportFixture.checkpointID, to: ImportFixture.realm)
    #expect(try store.feed(Fixture.feedID).babyID == Fixture.sharedBabyID)
}

@Test func test_editsAfterManifestFreezeBecomeOrderedPostImportIntents() throws {
    let fixture = try ImportFixture.storeWithFeedAndTimer()
    let frozen = try fixture.store.freezeImportManifest(checkpointID: fixture.checkpointID)
    let edit = try fixture.store.editFeed(Fixture.feedID, volumeML: 90)
    let stop = try SyncIntentTestAdapter(store: fixture.store).stopFeed(timerID: Fixture.timerID)
    #expect(frozen.sourceBases[EntityKey(kind: "feed", id: Fixture.feedID)] == Fixture.originalFeedPayload)
    #expect(try fixture.store.postImportIntents(checkpointID: fixture.checkpointID).map(\.id) == [edit.mutationID, stop.mutationID])
}

@Test func test_lostManifestChunkRetryUsesFrozenBytesAfterLocalDelete() async throws {
    let fixture = try ImportFixture.storeWithFeedAndTimer()
    let frozen = try fixture.store.freezeImportManifest(checkpointID: fixture.checkpointID)
    _ = try fixture.store.deleteFeed(Fixture.feedID)
    try await fixture.coordinator.retryManifestChunk(ordinal: 0)
    #expect(try fixture.transport.sentManifestChunk(ordinal: 0).manifestHash == frozen.chunks[0].manifestHash)
}

@Test func test_preFreezeCreateDeleteAndTimerSwitchAreCapturedInOneManifestRevision() throws {
    let fixture = try ImportFixture.storeWithFeedAndTimer()
    let created = try fixture.store.logDiaper(for: Fixture.localBabyID)
    _ = try fixture.store.deleteFeed(Fixture.feedID)
    _ = try fixture.store.switchFeedTimer(Fixture.timerID, to: .right)
    let frozen = try fixture.store.freezeImportManifest(checkpointID: fixture.checkpointID)
    #expect(frozen.sourceBases[EntityKey(kind: "diaper", id: created.id)] != nil)
    #expect(frozen.sourceBases[EntityKey(kind: "feed", id: Fixture.feedID)] == nil)
    #expect(frozen.chunks.flatMap(\.manifestPayload).contains(Fixture.rightSideTimerPayloadByte))
}

@Test func test_editBetweenMappingAndRewriteAndWriteDuringDrainingFollowFrozenBaseline() throws {
    let fixture = try ImportFixture.storeWithFeedAndTimer()
    _ = try fixture.store.freezeImportManifest(checkpointID: fixture.checkpointID)
    try fixture.store.saveMapping(fixture.resolverResponse, checkpointID: fixture.checkpointID)
    let between = try fixture.store.editFeed(Fixture.feedID, volumeML: 90)
    try fixture.store.rewriteAndMoveWorkspace(checkpointID: fixture.checkpointID, to: fixture.realm)
    let draining = try fixture.store.logDiaper(for: Fixture.sharedBabyID)
    #expect(try fixture.store.sharedOutboxIDs(in: fixture.realm).suffix(2) == [between.mutationID, draining.mutationID])
}

@Test func test_createFamilyPreservesDistinctDuplicateLocalBabiesAndJoinStopsAmbiguity() async throws {
    let fixture = try ImportFixture.twoLocalBabiesSameNormalizedNameAndDate()
    let create = try fixture.coordinator.makeResolveRequest(importMode: .preserveLocalIDs, chunkOrdinal: 0)
    #expect(create.importMode == .preserveLocalIDs)
    #expect(try await fixture.server.resolve(create).mapping.localBabyIDs.count == 2)
    let join = try fixture.coordinator.makeResolveRequest(importMode: .matchShared, chunkOrdinal: 0)
    await #expect(throws: ImportConflictError.ambiguousLocalBabies) { try await fixture.server.resolve(join) }
}

@Test func test_ownerConflictChoiceRequiresInjectiveFinalMapping() throws {
    let response = ImportFixture.mappingWithTwoLocalIDsToOneSharedID
    #expect(throws: ImportStoreError.nonInjectiveMapping) { try ImportStoreValidator.validate(response) }
    let choice = OwnerConflictChoice.useExisting
    #expect(ImportConflictResolution(choice: choice, canonicalBabyID: Fixture.sharedBabyID).isValid)
    #expect(ImportConflictResolution(choice: .keepLocal, canonicalBabyID: nil).isValid)
}
```

- [ ] **Step 2: Run focused RED tests and verify current code has no durable import state machine (2–5 minutes)**

Run: `rtk swift test --package-path Packages/BabyTracker --filter 'ImportStoreTests|ImportCoordinatorTests|ImportConflictViewModelTests'`

Expected: FAIL because there is no checkpointed frozen P2 manifest/source base, post-import intent queue, resolver client, mapping persistence, guarded rewrite, or owner-conflict UI.

- [ ] **Step 3: Implement the phase checkpoint and single-transaction workspace rewrite (2–5 minutes)**

```swift
func rewriteAndMoveWorkspace(checkpointID: UUID, to realm: RealmID) throws {
    guard let token = try leaseStore.acquire(scope: .realm(activeRealm), ownerID: processID, now: clock.now, ttl: 30) else {
        throw RealmWriteError.retryAfterTransition
    }
    defer { try? leaseStore.cancel(token) }
    try leaseStore.withFencedCommit(token, now: clock.now) {
        try context.transaction {
            let checkpoint = try requiredCheckpoint(checkpointID, phase: .mappingDurable)
            let manifest = try requiredFrozenManifest(checkpointID)
            try createImportMutations(from: manifest, checkpoint: checkpoint, realm: realm)
            try remapBabiesAndDependents(checkpoint.completeBabyIDMap, into: realm)
            try moveRows([Baby.self, FeedEntry.self, SleepEntry.self, DiaperEntry.self, ActiveTimer.self], from: .local, to: realm)
            try movePostImportIntents(checkpointID: checkpointID, afterImportedManifest: manifest, into: realm)
            checkpoint.phase = .sharedRealmExposed
            try activateRealmAndAdvanceTransitionGeneration(realm, checkpointID: checkpointID)
        }
    }
}
```

Use P2’s versioned normalizer/result in the preview and request; do not implement fuzzy matching. Set `import_mode: preserve_local_ids` for create-family and `import_mode: match_shared` for join in chunk 0, persist it in `ImportCheckpoint`, and reject any resume/chunk whose mode differs. In create-family mode retain every local baby UUID, including two local babies with equal normalized name/date. In join mode, surface P2 `ambiguous_local_babies` rather than collapsing local IDs. Validate that every final mapping is injective before persistence; owner resolution encodes only `choice: use_existing` with a canonical baby UUID or `choice: keep_local` with `canonical_baby_id: null`. In one local transaction at `.membershipReady`, capture each live entity's immutable versioned protocol payload/hash and ordinal into `FrozenImportManifest`, persist chunk payload/hash plus source revision/base in `ImportCheckpoint`, and set `.manifestFrozen` before sending any resolver/chunk request. Retry a lost chunk with those exact bytes/hash. Before freeze, edits/creates/deletes/timer actions are included by rebuilding the still-unsubmitted manifest transactionally. From `.manifestFrozen` through `.draining`, immediate offline writes remain visible but are appended as ordered post-import intents after the frozen baseline; they never mutate the P2 manifest payload or hash. Persist the complete mapping before changing any baby/dependent reference. Matching shared babies adopt the P2 canonical UUID; unmatched babies and every record keep their UUID. Under the P3.1 fenced realm lease, rewrite `FeedEntry`, `SleepEntry`, `DiaperEntry`, and `ActiveTimer` baby IDs, move rows, enqueue frozen import mutations first, then enqueue post-import edit/create/delete/timer intents in source order, all in one transaction before exposing the shared realm. Equal existing record UUID/content is a server-confirmed no-op; unequal content becomes the explicit P2 conflict state. Show the saved displayed legacy date in `ImportSummaryView`; show an owner-resolution-only conflict and resume the same checkpoint after resolution. Initial imported attribution identifies the importing caregiver while retaining domain timestamps.

Every resolver/status/conflict request uses exactly
`records-v1,import-resolver-v1`. Persist `conflict_count`,
`conflicts_complete`, and `next_conflict_page_token` with the resolver revision;
drain conflict pages in UUID order under the same item/byte bounds before
presenting a complete list. Treat `import_resolution_stale` from a continuation
or owner choice as a command to discard only the paged conflict projection and
restart status at page one, never to discard the frozen manifest or refreeze
live edits. Enforce the 512-byte token maximum before transport. The client does
not infer conflict completeness from `mapping_complete`.

Freeze active timers as timer-start manifest entries with their original start
content hash and local baby ID. After the durable mapping, submit only imported
timer starts; switches, bottle attachments, and stops captured before freeze are
represented in the frozen start state where permitted by P2, while later
switch/attach/stop actions remain ordered non-import post-import intents.
Completed timers enter the manifest as their completed feed/sleep record and
never request import equality treatment for a timer completion. Imported record
updates/deletes are never synthesized.

Extend `BabyStoreLeaseProbe/main.swift` and `ImportStoreTests.swift` with the deterministic two-process race above: the widget process captures the old write context, the app process prepares the import rewrite, and a nonblocking resume signal lets the widget call the real `BabyStore` diaper-save API while the rewrite is pending. Wait for both probes to exit before inspecting the durable database; assert zero partial/wrong-realm diaper or outbox rows, then one correct row/intent after an explicit fresh-context retry. Also run the reverse ordering where the widget commits first and the rewrite moves its whole row/intent. No test calls `syncOnce` to obtain the lock for the original local save. Import storage owns injected `FencedLeaseStore`, process ID and clock; its rewrite acquires/cancels the source-realm token as shown and updates the durable realm and generation in the same commit. Manifest freezing/post-import writes use the local write boundary; only the short exclusive rewrite phase rejects writers.

- [ ] **Step 4: Run focused GREEN, UI, and full package suite (2–5 minutes)**

Run: `rtk swift test --package-path Packages/BabyTracker --filter 'ImportStoreTests|ImportCoordinatorTests|ImportConflictViewModelTests' && rtk make test && rtk xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -only-testing:BabyTrackerUITests/FamilyImportUITests`

Expected: PASS; create-family emits immutable `preserve_local_ids` and retains distinct duplicate local babies, join emits immutable `match_shared` and blocks P2 `ambiguous_local_babies`, noninjective mapping is rejected before rewiring, and only the exact owner-conflict choices are encodable. Kill/relaunch before manifest freeze, after freeze, after a lost chunk response, after mapping, during rewrite, and during draining resumes with one mapping; create/edit/delete/timer side-or-stop actions before freeze are captured in the frozen manifest, later actions are ordered post-import intents, no partial realm is visible, all dependent references move atomically, no duplicate records are heuristically merged, and writes made while draining join the shared outbox without self-conflict.

- [ ] **Step 5: Commit durable import onboarding (2–5 minutes)**

```bash
rtk git add Packages/BabyTracker/Sources/BabySync/ImportCoordinator.swift Packages/BabyTracker/Sources/BabySync/AccountCoordinator.swift Packages/BabyTracker/Sources/BabySync/FamilyClient.swift Packages/BabyTracker/Sources/BabySync/ServerSelection.swift Packages/BabyTracker/Sources/BabyStore/ImportStore.swift Packages/BabyTracker/Sources/BabyStore/BabyStore+Import.swift Packages/BabyTracker/Sources/BabyStore/ImportManifestSnapshot.swift Packages/BabyTracker/Sources/SettingsFeature/ImportSummaryView.swift Packages/BabyTracker/Sources/SettingsFeature/ImportConflictView.swift Packages/BabyTracker/Sources/SettingsFeature/FamilySettingsViewModel.swift Packages/BabyTracker/Sources/SettingsFeature/FamilySettingsView.swift Packages/BabyTracker/Tests/BabyStoreTests/ImportStoreTests.swift Packages/BabyTracker/Tests/BabyStoreTests/SyncIntentTestAdapter.swift Packages/BabyTracker/Sources/BabyStoreLeaseProbe/main.swift Packages/BabyTracker/Tests/BabySyncTests/ImportCoordinatorTests.swift Packages/BabyTracker/Tests/SettingsFeatureTests/ImportConflictViewModelTests.swift UITests/BabyTrackerUITests/FamilyImportUITests.swift Makefile
rtk git commit -m "feat: import local data into family realm"
```

### Task 5: HTTPS sync, base-plus-overlay reconciliation, snapshots, recovery, and revocation (P3.5)

**Files:**

- Replace: `Packages/BabyTracker/Sources/BabySync/{SyncClient,LocalOnlySync}.swift`
- Create: `Packages/BabyTracker/Sources/BabySync/{FamilySyncHTTPClient,SyncCoordinator,SyncRetryPolicy,SessionRefresher,RecoveryCoordinator,MembershipRevocationHandler,SyncIssueStore}.swift`
- Modify: `Packages/BabyTracker/Package.swift`, `Packages/BabyTracker/Sources/BabySync/KeychainSessionStore.swift`
- Create: `Packages/BabyTracker/Sources/BabySyncSessionProbe/main.swift`
- Create: `Packages/BabyTracker/Sources/BabyStore/{RemoteApplyStore,BabyStore+RemoteApply,BabyStore+Snapshot}.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/{OverlayMaterializer,BabyStore+Outbox}.swift`
- Create: `Packages/BabyTracker/Tests/BabySyncTests/{SyncCoordinatorTests,SyncRetryPolicyTests,RecoveryCoordinatorTests,DeviceContinuityRecoveryTests}.swift`
- Create: `Packages/BabyTracker/Tests/BabySyncTests/{SessionRefresherTests,TwoProcessSessionHarness}.swift`
- Create: `Packages/BabyTracker/Tests/BabyStoreTests/{RemoteApplyTests,SnapshotApplyTests,RevocationPurgeTests}.swift`

**Interfaces:**

- Consumes: the exact P2 sync/snapshot requests, `SyncResponse`, `SnapshotResponse`, individual `Outcome` statuses, fixed high-water cursors, recovery epoch, and P1 session refresh/logout/revocation errors.
- Consumes: P3.1 `FencedLeaseStore` for `.realm(activeRealm)` cursor/import commits and `.account(accountSessionKey.leaseKey)` refresh serialization through P3.3's adapter, plus P3.1's durable `RefreshAttemptState` and P3.3 `DeviceContinuity`/`FamilySessionKey`/local `SessionCredentials.credentialVersion`.
- Produces:

```swift
public enum SyncReason: Sendable { case launch, foreground, localWrite, liveHint, backgroundPush, extensionRequest }
@MainActor public final class SyncCoordinator {
    public func syncOnce(reason: SyncReason) async
    public func startForegroundSync() async
    public func stopForegroundSync() async
}

@MainActor public extension BabyStore {
    func applySyncPage(_ page: SyncResponse) throws
    func applySnapshotPage(_ page: SnapshotResponse) throws
    func finishSnapshot(snapshotID: UUID) throws
    func purgeSharedRealm(_ realm: RealmID) throws
}
```

- [ ] **Step 1: Write failing M1/M2, high-water, retry, recovery, and revocation tests (2–5 minutes)**

```swift
@Test func test_fixedHighWaterCursorWaitsForAllPages() throws {
    let store = try BabyStore.sharedFixture(cursor: Fixture.cursor10)
    try store.applySyncPage(Fixture.page11Through15(nextPageToken: "page-2", highWater: Fixture.cursor20))
    #expect(try store.cursor == Fixture.cursor10)
    try store.applySyncPage(Fixture.page16Through20(nextPageToken: nil, highWater: Fixture.cursor20))
    #expect(try store.cursor == Fixture.cursor20)
}

@Test func test_m1ResponseAfterLocalM2DoesNotRegressOverlay() throws {
    let store = try BabyStore.sharedFixture()
    let (m1, m2) = try store.enqueueTwoEditsForSameFeed()
    try store.applySyncPage(Fixture.responseFor(m1, canonicalVolume: 60))
    #expect(try store.feed(Fixture.feedID).volumeML == 90)
    #expect(try store.pendingMutationIDs(for: Fixture.feedID) == [m2])
}

@Test func test_stableRevocationPurgesAfterAuthorizationFailure() async throws {
    let fixture = try SyncFixture.sharedRealmWithLocalOnlyRows()
    await fixture.coordinator.receive(Fixture.revokedMembershipResponse)
    #expect(try fixture.store.rows(in: fixture.realm).isEmpty)
    #expect(try fixture.store.rows(in: .local).isEmpty == false)
    #expect(try fixture.keychain.loadFamily(fixture.familyKey) == nil)
}

@Test func test_recoveryEpochDoesNotReuploadAcknowledgedMutation() throws {
    let store = try BabyStore.sharedFixture(acknowledgedMutationID: Fixture.m1)
    try store.applySnapshotPage(Fixture.recoveryEpoch2SnapshotWithoutM1)
    try store.finishSnapshot(snapshotID: Fixture.recoveryEpoch2SnapshotWithoutM1.snapshotID)
    #expect(try store.nextDispatchBatch(limit: 10).contains(where: { $0.id == Fixture.m1 }) == false)
}

@Test func test_lostStartResponseThenFreshAppleProofRecoversExactReceiptBeforeStop() async throws {
    let fixture = try DeviceContinuityFixture.frozenStartWithLostResponse()
    try await fixture.coordinator.syncOnce(reason: .localWrite)
    try await fixture.coordinator.handleExpiredAccessWithFreshAppleProof()
    #expect(fixture.lastAppleExchange.deviceContinuity == fixture.familyCredentials.deviceContinuity)
    #expect(try fixture.store.persistedDispatch(mutationID: Fixture.startMutationID).deliveryState == .acknowledged)
    #expect(try fixture.store.nextDispatchBatch(limit: 10).map(\.id) == [Fixture.dependentStopMutationID])
}

@Test func test_uncertainRefreshTakeoverUsesFreshProofWithoutReusingParentToken() throws {
    let harness = try TwoProcessSessionHarness.frozenStartReceiptAndExpiredAccess()
    let app = try harness.launchProbe(ownerID: Fixture.appOwner, action: .persistAttemptRefreshThenPauseBeforeCredentialCommit)
    let pending = try harness.store.refreshAttemptState(for: harness.accountKey.leaseKey)
    guard case .inFlightUncertain = pending else { Issue.record("Expected persisted uncertain refresh attempt"); return }
    let widget = try harness.runProbe(ownerID: Fixture.widgetOwner, action: .takeExpiredLeaseFreshAppleProofAndCommit)
    #expect(widget.result == .freshAppleProofCommitted)
    #expect(harness.server.refreshRequestCount == 1) // Only the old holder sent the parent token.
    #expect(harness.server.refreshTokenReuseRequestCount == 0)
    #expect(harness.server.lastAppleExchangeDeviceID == harness.originalDeviceID)
    #expect(harness.server.lastAppleExchangeUsedBoundDeviceContinuity)
    #expect(try harness.store.persistedDispatch(mutationID: Fixture.startMutationID) == harness.originalFrozenStartDispatch)
    #expect(try harness.store.refreshAttemptState(for: harness.accountKey.leaseKey) == .idle)
    let installed = try harness.keychain.loadFamily(harness.familyKey)
    try app.resume(action: .fencedCommit)
    try app.waitForExit()
    #expect(app.result == .staleLease)
    #expect(try harness.keychain.loadFamily(harness.familyKey) == installed)
    #expect(try harness.store.refreshAttemptState(for: harness.accountKey.leaseKey) == .idle)
    try harness.resumeSyncAndRecoverExactStartReceipt()
    #expect(harness.server.receiptDeviceID(for: Fixture.startMutationID) == harness.originalDeviceID)
    #expect(try harness.store.persistedDispatch(mutationID: Fixture.startMutationID).deliveryState == .acknowledged)
    #expect(try harness.store.nextDispatchBatch(limit: 10).map(\.id) == [Fixture.dependentStopMutationID])
}
```

- [ ] **Step 2: Run focused RED tests and confirm the timestamp-based seam is inadequate (2–5 minutes)**

Run: `rtk swift test --package-path Packages/BabyTracker --filter 'SyncCoordinatorTests|SyncRetryPolicyTests|RecoveryCoordinatorTests|DeviceContinuityRecoveryTests|SessionRefresherTests|RemoteApplyTests|SnapshotApplyTests|RevocationPurgeTests'`

Expected: FAIL because legacy `SyncClient.push/pull(since: Date?)` has no P2 cursor pages, receipt outcome, snapshot, recovery epoch, or realm purge semantics.

- [ ] **Step 3: Implement ordered dispatch and remote base application without echo mutations (2–5 minutes)**

Every `dispatchAndPull` path consults `SessionRefresher.swift` before ordinary sync bytes leave, even if a cached access token has not yet expired; the account authorization gate and unresolved refresh marker are separate checks. The refresher first reads P3.1's attempt marker under the account fence. When refresh is needed and the marker is idle, load the matching Keychain credential version and persist `beginRefreshAttempt` before invoking refresh transport. On a successful response, call `finishRefreshAttempt` with the current token and matching attempt ID; its install closure updates the one Keychain item through `KeychainSessionStore.swift`. If refresh may have been sent but no credential commit completed, retain uncertainty and frozen outbox bytes/IDs. A process taking an expired generation that sees `.inFlightUncertain` must not send the stored parent refresh token: obtain a new P1 challenge, construct D6's nonce-bound fresh Apple proof, include the retained origin/account/device continuity, require the original DeviceID, and use its current account fence to install the new credentials and clear that same marker. Before that proof completes, keep sync/dependent timers blocked and preserve pending intents. A widget/extension that cannot present Apple sign-in schedules foreground authentication and waits; the two-process test injects P1's compile-time-only proof provider to exercise completion. Reject any old response using the old lease before it can overwrite credentials or clear state. A definitive failure before transport invocation may restore idle; cancellation/timeout/lost response after possible send may not.

Add `BabySyncSessionProbe` as an executable target depending on `BabySync` and `BabyStore` in `Package.swift`; unlike the earlier store-only probe it owns the test refresh/auth/Keychain-access flow. `TwoProcessSessionHarness.swift` launches two independent probe processes against a shared temporary App Group and isolated test Keychain namespace, with P1's existing compile-time-only issuer/server fixture and a controllable clock. Its first process pauses after P1 rotates the parent refresh token but before local installation; the second observes the durable marker after lease expiry, commits fresh Apple proof, and never calls refresh with the parent. `SessionRefresherTests.swift` owns the test above plus definitive-pre-send vs ambiguous failure, process restart, and crash-after-Keychain-install-before-marker-clear cases. Assert one refresh request total, zero reuse requests, unchanged original DeviceID/frozen dispatch, stale-old-holder rejection, and exact old-device receipt recovery before a dependent stop becomes dispatchable. Test transport inspection uses counters/IDs only, never retained/logged token values. Reuse the existing P1 issuer contract; do not add another issuer or move the store-only probe onto `BabySync`.

```swift
func applySyncPage(_ page: SyncResponse) throws {
    try context.transaction {
        try requireMatchingServerIdentity(page.serverInstanceID, page.recoveryEpoch)
        for outcome in page.outcomes { try resolveOnlyNamedReceipt(outcome) }
        for change in page.changes { try applyNonRegressingCanonicalBase(change) }
        try OverlayMaterializer.refoldPendingIntents(in: context, realm: activeRealm)
        try persistPageProgress(page)
        if page.nextPageToken == nil { try advanceCursor(to: page.highWater) }
    }
}

func syncOnce(reason: SyncReason) async {
    guard (try? persistence.accountAllowsOrdinarySync(accountSessionKey.leaseKey)) == true else { return }
    guard let token = try? leaseStore.acquire(scope: .realm(realm), ownerID: ownerID, now: clock.now, ttl: 30) else { return }
    defer { try? leaseStore.cancel(token) }
    do {
        let renewed = try leaseStore.renew(token, now: clock.now, ttl: 30)
        let response = try await dispatchAndPull(reason: reason, refreshLeaseScope: .account(accountSessionKey.leaseKey))
        try leaseStore.withFencedCommit(renewed, now: clock.now) { try persistence.applySyncPage(response) }
    } catch FencedLeaseError.staleOwner { return }
      catch is CancellationError { return }
      catch { issueStore.record(error, realm: realm) }
}
```

Mark/freeze selected mutations before bytes leave, submit bounded ordered batches, and retain pending rows on network/timeout/429/5xx under deterministic jitter. Never await transport while holding the P3.1 commit lock; acquire/renew a realm token before request and verify that same generation inside each `applySyncPage`, snapshot, purge, and import-transition commit. Cancellation releases its token; a stale owner abandons its response without changing cursor/base/outbox and requests a later bounded sync. On expired access, acquire/renew the account-scope lease and reread the persisted attempt state. Only `.idle` may persist a new `.inFlightUncertain` attempt and send one refresh request before one bounded sync retry. An uncertain attempt, including after lease takeover, never resends the stored refresh token; it preserves frozen outbox rows and requires fresh nonce-bound Apple proof with device continuity before resuming. If P1 requires fresh Apple proof, load the Keychain `DeviceContinuity`, send the exact optional `device_continuity` DTO, and retain the same server device ID before retrying the frozen mutation. Missing/invalid continuity is a visible terminal sync issue; never dispatch a dependent timer command, create a fallback device, or retarget a receipt. Quarantine only the stable permanently rejected mutation and let unrelated work continue. `applied`, `already_applied`, and `superseded` resolve only the named mutation ID; P3.2 inspects `already_applied.original_status` before releasing timer dependents. Remote apply updates a non-regressing `(epoch, entityVersion)` base and refolds later overlays without appending an outbox row. Invalid cursor obtains an authenticated fixed snapshot. A changed recovery epoch stops incremental advancement, waits for authorization readiness/reauthentication, atomically replaces bases via staged snapshot, preserves only never-acknowledged intents in original dependency order, and never reuploads acknowledged state absent after restore. A stable revocation stops transport, deletes the exact realm’s Keychain data and cached rows/base/outbox/cursor, ends its live activities, reloads widgets, and activates `.local`.

`SyncCoordinator` is a `@MainActor` class, matching `FencedLeaseStore` and `SyncPersistence`; its implementation and store/coordinator tests use that isolation consistently. Async URLSession transport suspends without holding the OS lock, then resumes on the main actor before the synchronous fenced commit. `defer` can synchronously cancel its token on success, failure or cancellation. Background/extension/WebSocket callers use `await coordinator.syncOnce` to cross to this actor; no synchronous cross-actor store call is permitted. Recheck the durable account gate before each transport/refresh request; signed-out generation invalidation rejects any earlier response at commit.


- [ ] **Step 4: Run focused GREEN and full package suite (2–5 minutes)**

Run: `rtk swift test --package-path Packages/BabyTracker --filter 'SyncCoordinatorTests|SyncRetryPolicyTests|RecoveryCoordinatorTests|DeviceContinuityRecoveryTests|SessionRefresherTests|RemoteApplyTests|SnapshotApplyTests|RevocationPurgeTests' && rtk make test`

Expected: PASS; partial pages retain the old cursor, M1 cannot erase M2, lower base versions regress nowhere, duplicate/remote records emit no mutation, two processes persist refresh uncertainty before send, takeover uses fresh Apple proof without any parent-token reuse, and stale completions cannot change credentials or clear attempt state, a lost timer-start response survives token expiry/fresh Apple proof and releases its exact dependent stop only after the old-device receipt, retry/refresh limits hold, quarantine is isolated, snapshot/recovery behavior is deterministic, and revocation leaves local-only rows intact.

- [ ] **Step 5: Commit protocol-accurate offline sync (2–5 minutes)**

```bash
rtk git add Packages/BabyTracker/Package.swift Packages/BabyTracker/Sources/BabySync Packages/BabyTracker/Sources/BabySyncSessionProbe/main.swift Packages/BabyTracker/Sources/BabyStore Packages/BabyTracker/Tests/BabySyncTests Packages/BabyTracker/Tests/BabyStoreTests Makefile
rtk git commit -m "feat: synchronize family realm mutations"
```

### Task 6: Foreground hints, bounded extension sync, and rendered activity gates (P3.6)

**Files:**

- Create: `Packages/BabyTracker/Sources/BabySync/{FamilySyncWebSocket,SyncScheduler,BackgroundSync}.swift`
- Modify: `Apps/BabyTrackerApp/{AppEnvironment,RootView,BabyTrackerApp}.swift`
- Modify: `Apps/BabyTrackerApp/UITestSupport.swift`, `UITests/BabyTrackerUITests/UITestHarness.swift`
- Create: `TestSupport/FamilySync/FamilySyncTestServer.swift`
- Modify: `Apps/SharedIntents/{WidgetStore,LogIntents}.swift`
- Modify: `Apps/BabyTrackerWidgets/*`
- Modify: `Packages/BabyTracker/Sources/BabyStore/LiveActivityController.swift`
- Modify: `Apps/BabyTracker.entitlements`, `BabyTracker.xcodeproj/project.pbxproj`
- Create: `Packages/BabyTracker/Tests/BabySyncTests/{FamilySyncWebSocketTests,BackgroundSyncTests}.swift`
- Create: `Packages/BabyTracker/Tests/BabyStoreTests/CrossProcessRealmTests.swift`
- Create: `UITests/BabyTrackerUITests/{FamilySyncRenderedStateUITests,FamilySyncTestServerTests}.swift`

**Interfaces:**

- Consumes: P2 `/v1/live` cursor-only hints, P3.1 `FencedLeaseStore`/transition generation, P3.5 `SyncCoordinator.syncOnce(reason:)`, and P3.3 Keychain access-group session accessor and durable signed-out account gate.
- Produces: reusable test-only `FamilySyncTestServer` in `TestSupport/FamilySync/FamilySyncTestServer.swift`, compiled into the existing `BabyTrackerUITests` target in this task before its rendered tests use it; P3.7 adds this same source to its E2E target, with no second implementation or issuer.
- Produces:

```swift
public actor FamilySyncWebSocket {
    public func start(realm: RealmID) async
    public func stop() async
}
```

- [ ] **Step 1: Write failing lease, hint-reorder, and rendered UI tests (2–5 minutes)**

```swift
func test_remoteTimerEndIsRenderedAfterCommittedApply() throws {
    let app = Harness.launch(.familySyncSharedFixture)
    FamilySyncTestServer.commitTimerTombstone(for: Fixture.timerID)
    XCTAssertTrue(app.staticTexts["No timer running"].waitForExistence(timeout: 15))
}

func test_sharedTestServerCommitsThroughProductionRoutes() throws {
    let server = try FamilySyncTestServer.isolatedFixture()
    try server.commitTimerTombstone(for: server.fixture.timerID)
    XCTAssertTrue(try server.fetchSnapshot().timerTombstones.contains(server.fixture.timerID))
    XCTAssertTrue(server.launchArgumentsContainFixtureIdentifiersOnly)
}
```

- [ ] **Step 2: Run focused RED tests and verify notifications alone are not an acceptance gate (2–5 minutes)**

Run: `rtk swift test --package-path Packages/BabyTracker --filter 'AppGroupFencedLeaseTests|FamilySyncWebSocketTests|BackgroundSyncTests|CrossProcessRealmTests' && rtk xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -only-testing:BabyTrackerUITests/FamilySyncTestServerTests -only-testing:BabyTrackerUITests/FamilySyncRenderedStateUITests`

Expected: FAIL because no P3.1 fenced lease consumer, socket lifecycle, bounded extension entry point, or real rendered remote-commit path exists.

- [ ] **Step 3: Implement the App Group lease and lifecycle ownership rules (2–5 minutes)**

```swift
func handleHint(_ hint: LiveHint) async {
    guard hint.familyID == realm.familyID else { return }
    await coordinator.syncOnce(reason: .liveHint)
}

func requestExtensionSync() async {
    await coordinator.syncOnce(reason: .extensionRequest)
}
```

Every widget/intent/activity domain save first uses P3.1's `withRealmWrite` through P3.2's public `BabyStore` write methods; its short OS lock and transition-generation check protect the entity/outbox transaction before any optional sync request. P3.6 consumes P3.4's real widget-save/import race in `CrossProcessRealmTests.swift` and checks retry/refresh after generation changes. P3.6 never bypasses a lease: each extension sync request and lifecycle action enters P3.5, which acquires/renews the P3.1 realm or account fenced token and commits only under its current generation. A process that resumes after another process takes an expired generation receives `staleLease` and must discard its response; a cancelled task releases its token. The foreground app alone opens `/v1/live` only after `/v1/server` advertises `live-hints-v1`; it sends exact `records-v1,live-hints-v1`, stops in background, reconnects with bounded exponential backoff, and always HTTPS-syncs after reconnect or hint. Without the capability it uses HTTPS polling only. Widget, App Intent, and Live Activity commit through `BabyStore`, may ask for exactly one bounded sync using access-group Keychain lookup, and never retain a socket/cursor interpretation loop. Launch, foreground, and hosted push schedule bounded catch-up. Refresh widgets/Live Activities only after a remote SwiftData transaction commits. Add APNs/background modes only when the Plan 4 hosted delivery path is wired; self-host remains launch/foreground/best-effort background refresh.

Create the shared `FamilySyncTestServer` driver now, with isolated fixture creation, real authenticated production-route timer commits, and snapshot verification. `FamilySyncTestServerTests.swift` owns its smoke checks. Wire its single source into `BabyTrackerUITests` using `project.pbxproj`; it is never a production app/extension source. Extend `UITestHarness.swift` and the app's debug-only `UITestSupport.swift` now so `.familySyncSharedFixture` can launch against that driver using non-secret fixture identifiers. Reuse P1's compile-time-only test issuer and production Go/PostgreSQL route stack; do not add another issuer, fake authoritative commit path, or require the future E2E target. P3.7 extends the driver for conflicts/fault injection and uses the same fixture/issuer plumbing.

- [ ] **Step 4: Run focused GREEN, rendered UI, and complete package suite (2–5 minutes)**

Run: `rtk swift test --package-path Packages/BabyTracker --filter 'AppGroupFencedLeaseTests|FamilySyncWebSocketTests|BackgroundSyncTests|CrossProcessRealmTests' && rtk xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -only-testing:BabyTrackerUITests/FamilySyncTestServerTests -only-testing:BabyTrackerUITests/FamilySyncRenderedStateUITests && rtk make test`

Expected: PASS; competing app/widget/intent work cannot interleave cursor or transition commits, a suspended stale owner cannot apply after generation takeover, reordered hints converge through HTTPS, extension sync stays bounded, and a foreground widget write plus a remotely ended timer are observed in rendered app/widget/activity state rather than only notification callbacks.

- [ ] **Step 5: Commit process-safe lifecycle integration (2–5 minutes)**

```bash
rtk git add Packages/BabyTracker/Sources/BabySync Apps/BabyTrackerApp Apps/SharedIntents Apps/BabyTrackerWidgets Packages/BabyTracker/Sources/BabyStore/LiveActivityController.swift Apps/BabyTracker.entitlements BabyTracker.xcodeproj/project.pbxproj Packages/BabyTracker/Tests/BabySyncTests Packages/BabyTracker/Tests/BabyStoreTests UITests/BabyTrackerUITests TestSupport/FamilySync/FamilySyncTestServer.swift Makefile
rtk git commit -m "feat: coordinate family sync across app processes"
```

### Task 7: Attribution, role-aware conflict UI, accessibility regression, and two-client/signed-installation gates (P3.7)

**Files:**

- Create: `Packages/BabyTracker/Sources/SettingsFeature/{SyncIssueView,TimerConflictView}.swift`
- Modify: `Packages/BabyTracker/Sources/{HistoryFeature/EntryEditSheet.swift,HistoryFeature/EntryEditModel.swift,SettingsFeature/BabyEditSheet.swift,HomeFeature/RunningTimerView.swift,SettingsFeature/FamilySettingsView.swift}`
- Modify: `Apps/BabyTrackerApp/UITestSupport.swift`, `UITests/BabyTrackerUITests/UITestHarness.swift`, `Makefile`, `BabyTracker.xcodeproj/project.pbxproj`
- Create: `BabyTracker.xcodeproj/xcshareddata/xcschemes/BabyTrackerFamilySyncE2ETests.xcscheme`
- Create: `UITests/BabyTrackerUITests/{FamilySyncConflictUITests,FamilySyncAccessibilityUITests}.swift`
- Create: `E2ETests/BabyTrackerFamilySyncE2ETests/FamilySyncTwoClientProcessTests.swift`; `docs/verification/family-sync-signed-installations.md`
- Modify: `TestSupport/FamilySync/FamilySyncTestServer.swift`, `UITests/BabyTrackerUITests/FamilySyncTestServerTests.swift`
- Create: `Packages/BabyTracker/Tests/{HistoryFeatureTests/AttributionTests.swift,SettingsFeatureTests/SyncIssueViewModelTests.swift,HomeFeatureTests/TimerConflictPresentationTests.swift}`

**Interfaces:**

- Consumes: P3.2 timer outcomes/dependencies, P3.4 import conflict/checkpoint state, P3.5 `SyncIssueStore`, P1 role/lifecycle profile view, and P2 child-deletion/timer-tombstone outcomes.
- Consumes: P3.6's shared `TestSupport/FamilySync/FamilySyncTestServer.swift`; extend its existing driver and add source membership to `BabyTrackerFamilySyncE2ETests`, preserving its earlier UI-test membership and single issuer integration.
- Produces:

```swift
public struct SyncIssue: Identifiable, Sendable, Equatable {
    public let id: UUID
    public let realm: RealmID
    public let code: String
    public let mutationID: UUID?
    public let presentation: SyncIssuePresentation
}

public enum SyncIssuePresentation: Sendable, Equatable {
    case retrying
    case permanentRejection(message: String)
    case timerCollision(message: String)
    case importConflict(message: String)
    case deviceContinuityRequired(message: String)
    case revoked
}

@MainActor public protocol SyncIssuePresenting: AnyObject {
    var issues: [SyncIssue] { get }
    func dismiss(issueID: UUID)
}
```

- [ ] **Step 1: Write failing detail/role/conflict/accessibility and two-client tests (2–5 minutes)**

```swift
@Test func test_formerCaregiverIsShownOnlyInDetail() throws {
    let entry = AttributionFixture.entry(lastEditorName: "Former caregiver")
    #expect(EntryEditModel(entry: entry).attributionText == "Last edited by Former caregiver")
    #expect(HistoryItem.feed(entry).title.contains("Former caregiver") == false)
}

func test_caregiverCannotDeleteBabyAndLosingTimerExplainsConflict() throws {
    let app = Harness.launch(.caregiverSharedFixture)
    XCTAssertFalse(app.buttons["Delete baby"].exists)
    FamilySyncTestServer.returnTimerCollision(for: Fixture.startMutationID)
    XCTAssertTrue(app.staticTexts["Another caregiver already started this timer"].waitForExistence(timeout: 15))
}

func test_lostStartResponseReauthRecoversOldDeviceReceiptBeforeDependentStop() throws {
    let pair = FamilySyncTwoClientProcessFixture.make()
    pair.server.dropNextSyncResponseAfterCommit(for: Fixture.startMutationID)
    pair.firstClient.startTimerAndStopOffline()
    pair.server.expireAccessToken(for: pair.firstClient)
    pair.firstClient.completeFreshAppleProof()
    XCTAssertEqual(pair.server.receiptDeviceID(for: Fixture.startMutationID), pair.firstClient.originalDeviceID)
    XCTAssertEqual(pair.server.receivedMutationIDs(), [Fixture.startMutationID, Fixture.dependentStopMutationID])
}
```

- [ ] **Step 2: Run focused RED tests and verify current UI has no family-specific presentation contract (2–5 minutes)**

Run: `rtk swift test --package-path Packages/BabyTracker --filter 'AttributionTests|SyncIssueViewModelTests|TimerConflictPresentationTests' && rtk xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -only-testing:BabyTrackerUITests/FamilySyncConflictUITests -only-testing:BabyTrackerUITests/FamilySyncAccessibilityUITests`

Expected: FAIL because creator/editor attribution, role gating, permanent sync issues, and timer/import conflict identifiers are absent.

- [ ] **Step 3: Implement bounded presentation state and deterministic test harnesses (2–5 minutes)**

```swift
if viewerRole.canDeleteBaby {
    Button("Delete baby", role: .destructive) { viewModel.requestDelete() }
}

Text(entry.attribution.displayText)
    .accessibilityIdentifier("entry-attribution")

SyncIssueView(issue: issue)
    .accessibilityIdentifier("family-sync-issue-\(issue.id.uuidString)")
```

Keep creator/last-editor subtle and restricted to entry detail; keep history rows uncluttered. Show `Former caregiver` for pseudonymized attribution without an old display name. Hide/deny child deletion for caregivers; owner delete confirmation explains server-side dependent tombstones and timer cancellation. Present a losing timer, import conflict, quarantined mutation, and revoke/disconnect state visibly without fabricating a completed feed. Add stable accessibility IDs for endpoint confirmation, join confirmation, import progress/retry, conflict resolution, sync issue, and attribution. Extend the debug-only harness with fake `SecureSessionStore`, Apple adapter, clock, and transport backed by the production-disabled test issuer; pass fixture identifiers only, never credentials/secrets, as launch arguments. Add the dedicated `BabyTrackerFamilySyncE2ETests` Xcode test target in `project.pbxproj` and its shared scheme, compiling P3.6's existing `TestSupport/FamilySync/FamilySyncTestServer.swift` as its test-only client; it drives real production-container/PostgreSQL routes under the test-only issuer configuration. Add `test-family-sync-e2e` to `Makefile`, update `EXPECTED_TESTS` in the same commit as new package tests, and make its command invoke the new scheme rather than relying on an untracked local scheme.

- [ ] **Step 4: Run focused GREEN, full UI, two-client, signed-installation, build, and package gates (2–5 minutes)**

Run: `rtk swift test --package-path Packages/BabyTracker --filter 'AttributionTests|SyncIssueViewModelTests|TimerConflictPresentationTests' && rtk xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -only-testing:BabyTrackerUITests/FamilySyncConflictUITests -only-testing:BabyTrackerUITests/FamilySyncAccessibilityUITests && rtk make test-family-sync-e2e && rtk make test && rtk make test-ui && rtk make build`

Expected: PASS; local and shared flows retain feeds/sleeps/diapers/solids/combination feeds/date editing/volume units/widgets/activities, conflict copy is accessible and truthful, two isolated helper processes create/join/import/sync/revoke against the reported real container, a lost timer-start response survives fresh Apple proof and retrieves its old-device receipt before the dependent stop, and the package total is deliberate. Record two separate simulator installations when automation supports them and two signed physical-device runs as an external beta gate; do not infer either from this process test.

- [ ] **Step 5: Commit final iOS client UI and acceptance coverage (2–5 minutes)**

```bash
rtk git add Packages/BabyTracker/Sources/HistoryFeature Packages/BabyTracker/Sources/SettingsFeature Packages/BabyTracker/Sources/HomeFeature Apps/BabyTrackerApp/UITestSupport.swift UITests/BabyTrackerUITests E2ETests/BabyTrackerFamilySyncE2ETests TestSupport/FamilySync/FamilySyncTestServer.swift BabyTracker.xcodeproj/project.pbxproj BabyTracker.xcodeproj/xcshareddata/xcschemes/BabyTrackerFamilySyncE2ETests.xcscheme Packages/BabyTracker/Tests/HistoryFeatureTests Packages/BabyTracker/Tests/SettingsFeatureTests Packages/BabyTracker/Tests/HomeFeatureTests Makefile
rtk git commit -m "test: verify family sync client flows"
```

### Task 8: Read-only caregivers (P3.8)

**Goal:** A third family role that can see everything and change nothing, so a
grandparent, childminder or night nurse can be given the family's history
without being able to alter it.

**Why it is its own task:** the role is not a UI affordance. A client that
merely hides the log buttons is trivially bypassed by an older build, a widget,
an App Intent, or a replayed frozen mutation — so the server is the gate and
the client is the courtesy. Both halves ship together or the feature is a lie.

**Ordering:** independent of P3.4-P3.6 (import, sync transport, background). It
shares ground with P3.7's role-aware presentation, so it lands before P3.7 and
P3.7 builds on the vocabulary it introduces.

**Files:**

- Create: `Server/migrations/000014_viewer_role.sql`
- Modify: `Server/internal/database/roles.go`; the mutation authorization path; family/invitation handlers
- Modify: `Server/api/fixtures/v1/family.json`, `family-actors.json`, `family-invitation.json`, `errors.json`
- Create: `Server/tests/protocol/viewer_test.go`
- Create: `Packages/BabyTracker/Sources/BabySync/FamilyRole.swift`
- Modify: `Packages/BabyTracker/Sources/BabySync/{FamilyClient,AccountCoordinator}.swift`
- Create: `Packages/BabyTracker/Sources/BabyStore/WritePermission.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/{BabyStore,BabyStore+Outbox,AppGroupWriteBoundary}.swift`
- Modify: `Packages/BabyTracker/Sources/BabyStore/Models/AccountAccessState.swift`
- Modify: `Packages/BabyTracker/Sources/SettingsFeature/{FamilySettingsViewModel,FamilySettingsView,FamilySyncSettingsModel}.swift`
- Modify: `Packages/BabyTracker/Sources/HomeFeature`, `LoggingFeature` dock and quick-log entry points
- Create: `Packages/BabyTracker/Tests/BabyStoreTests/WritePermissionTests.swift`
- Create: `Packages/BabyTracker/Tests/SettingsFeatureTests/ReadOnlyCaregiverTests.swift`
- Create: `UITests/BabyTrackerUITests/ReadOnlyCaregiverUITests.swift`

**Interfaces:**

```swift
public enum FamilyRole: String, Codable, Sendable, CaseIterable {
    case owner, caregiver, viewer

    /// The single authority on whether this role may originate a mutation.
    /// Read by the store's write boundary, not by any view.
    public var canWrite: Bool { self != .viewer }
    public var canManageMembers: Bool { self == .owner }
    public var canInvite: Bool { self == .owner }
}

public enum WritePermissionError: Error, Sendable, Equatable { case readOnlyCaregiver }

@MainActor public extension BabyStore {
    /// Durable, per-realm, survives relaunch and is read by the widget and
    /// App Intents as well as the app.
    func setWritePermission(_ role: FamilyRole, for realm: RealmID) throws
    func writePermission(in realm: RealmID) throws -> FamilyRole
}

@MainActor public protocol FamilySyncSettingsModel: AnyObject {
    var role: FamilyRole { get }
    func inviteViewer() async throws -> FamilyInvitation
    func changeRole(of memberID: UUID, to role: FamilyRole) async throws
}
```

- [ ] **Step 1: Write failing server and client permission tests (2-5 minutes)**

```swift
@Test func test_viewerRealmRefusesEveryWritePathIncludingWidgetAndIntents() throws {
    let store = try BabyStore.sharedFixture()
    try store.setWritePermission(.viewer, for: Fixture.sharedRealm)
    #expect(throws: WritePermissionError.readOnlyCaregiver) { _ = try store.logDiaper(.wet, for: Fixture.babyID) }
    #expect(throws: WritePermissionError.readOnlyCaregiver) { _ = try store.startFeed(side: .left, for: Fixture.babyID) }
    #expect(try store.pendingOutbox().isEmpty)
}

/// The local realm is the user's own and is never gated by a family role.
@Test func test_viewerInSharedRealmStillWritesFreelyInLocalRealm() throws { }

/// Demotion must not strand work the caregiver had already queued.
@Test func test_demotionToViewerKeepsAlreadyQueuedMutationsDispatchable() throws { }

/// The gate is durable, so a widget process that never saw the demotion
/// still refuses.
@Test func test_writePermissionSurvivesRelaunchAndIsReadByProbeProcess() throws { }
```

```go
func TestViewerMutationsRejected(t *testing.T) {
    // POST /v1/sync from a viewer returns every outcome rejected with
    // code "read_only_caregiver", writes no journal row, and does not
    // advance the family cursor.
}
func TestViewerCannotInviteOrManageMembers(t *testing.T) {}
func TestOwnerCanChangeRoleBetweenCaregiverAndViewer(t *testing.T) {}
```

- [ ] **Step 2: Run the focused RED tests and confirm neither half enforces the role (2-5 minutes)**

Run: `rtk swift test --package-path Packages/BabyTracker --filter 'WritePermissionTests|ReadOnlyCaregiverTests'` and `rtk make server-test`

Expected: FAIL. `FamilyRole` does not exist, the store has no write gate, and the
database CHECK constraint rejects `viewer` outright.

- [ ] **Step 3: Implement the server gate, then the client courtesy (2-5 minutes)**

Server first, because it is the gate. `000014_viewer_role.sql` widens the
`family_members.role` CHECK to `('owner','caregiver','viewer')` and adds the
role to the invitation row; existing rows are untouched, so the migration is
additive and reversible. The mutation path rejects every mutation from a viewer
member with the stable code `read_only_caregiver`, writes no journal row, and
leaves the family cursor where it was — a rejected batch must not look like
progress. Viewers keep full read access: snapshot, sync pages and live hints are
unchanged, because the whole point is that they see everything.

Client second. `FamilyRole` is the one place that answers "may this role write";
no view makes that decision for itself. The role is persisted per realm on the
existing `AccountAccessState` row so it is durable and visible to the widget and
App Intents, which run in their own processes and would otherwise never learn
about a demotion. `AppGroupWriteBoundary` consults it inside the same lock that
already guards realm and generation, so the gate cannot be raced by a write
that started before the demotion landed. The local realm is never gated: it is
the user's own tracking and predates any family.

Already-queued mutations stay dispatchable after a demotion. The server will
reject them and that is the correct outcome — silently dropping a caregiver's
unsent work because their role changed would lose data they legitimately
recorded while they still had permission.

UI last. The dock, quick-log buttons and swipe-to-edit affordances are hidden
rather than disabled for a viewer: a row of greyed-out buttons invites tapping
and explains nothing. One sentence states the role and who can change it. The
owner's member list gains a role control, and the invite flow gains a
read-only option that produces the same link and QR as any other invitation.

- [ ] **Step 4: Run focused GREEN, full package, server, and rendered UI verification (2-5 minutes)**

Run: `rtk swift test --package-path Packages/BabyTracker --filter 'WritePermissionTests|ReadOnlyCaregiverTests' && rtk make test && rtk make build && rtk make server-test && rtk xcodebuild test -scheme BabyTracker -destination 'platform=iOS Simulator,name=iPhone 17 Pro' -only-testing:BabyTrackerUITests/ReadOnlyCaregiverUITests`

Expected: PASS; a viewer cannot originate a mutation from the app, the widget or
an App Intent; the server rejects one that reaches it anyway with
`read_only_caregiver` and advances no cursor; local-only tracking is unaffected;
and the viewer's Home renders without logging controls and with the explanation.

#### Status after the first pass

Done and verified without a database:

- Schema: `000014_viewer_role.sql` widens the membership and invitation role
  constraints and adds `app.membership_may_write`. Both CHECK constraints are
  dropped by lookup rather than by assumed name, because a wrong name would
  leave the old constraint in place, make the effective rule the AND of both,
  and report success while still rejecting `viewer`.
- Server gate: `access.RoleMayWrite` is the single write authority and fails
  closed on an unrecognised role; `sync.Apply` refuses a viewer inside the
  authorized transaction, so the rollback leaves no journal row and no advanced
  cursor; `family.allowed` grants viewers reads and nothing else; the HTTP layer
  maps it to a non-retryable `read_only_caregiver`.
- Client gate: `FamilyRole` is the single client authority, the permission is
  durable and per-realm on `RealmWritePermission`, and `AppGroupWriteBoundary`
  checks it inside the same lock that guards realm and generation — so a widget
  or App Intent that never saw the demotion still refuses.
- UI: the dock is hidden rather than disabled and one sentence says why and who
  can change it; the Caregivers screen gains the role control and a view-only
  invitation.
- Tests: `WritePermissionTests` (8), `ReadOnlyCaregiverTests` (9),
  `internal/access/roles_test.go`, `internal/family/viewer_role_test.go`, and
  four rendered `ReadOnlyCaregiverUITests`.

**Role administration: done.** `member_role_changed` is a full event-sourced
command — payload, registry entry, validator, projection handler, command
decision, service method, HTTP handler, route, and OpenAPI schema. It refuses
to touch an owner in both the decision and the projection, because a family
must always have exactly one and stepping down is `owner_transferred`, which
names a successor in the same command.

The server's own guards caught two omissions worth recording: an undocumented
route fails `TestOpenAPIRouteSetMatchesRegisteredRoutes`, and an unregistered
event kind fails `TestEventDescriptorRegistryIsComplete`. Both are now
satisfied rather than suppressed.

**Still owed:** `tests/protocol/viewer_test.go` skips until the deferred
end-to-end pass, since it needs live PostgreSQL; the unit coverage in
`internal/security/member_role_test.go`, `internal/access/roles_test.go` and
`internal/family/viewer_role_test.go` stands in the meantime.

- [ ] **Step 5: Commit read-only caregivers (2-5 minutes)**

```bash
rtk git add Server/migrations Server/internal Server/api/fixtures Server/tests Packages/BabyTracker/Sources Packages/BabyTracker/Tests UITests/BabyTrackerUITests Makefile
rtk git commit -m "feat: add read-only caregivers"
```

## Mandatory review amendments for P3 acceptance

These checklist items are part of their owning tasks even where an older code
sample above uses the superseded type name.

### P3.1 protocol, predecessor fixture, and process-safe persistence

- [ ] `BabyProtocol` implements the P2 canonicalizer, not ordinary
  `JSONEncoder` hashing: exact decimals/timestamps, baby-name normalization,
  safe integers, optional/null/empty normalization, nested duplicate rejection,
  and literal canonical bytes/hashes must match the one server corpus. A test
  support build step verifies its committed digest manifest and imports the
  server corpus into tests; release products ship no fixture resources.
- [ ] Before changing production model metadata, build the accepted predecessor
  app SHA and create a closed App-Group store with its actual `BabySchema`, all
  entities, nonempty photo bytes, timer, and change log. Commit the coordinated
  SQLite fixture plus producer SHA/toolchain/model manifest. Include WAL/SHM
  only for a deliberately validated crash-consistent WAL fixture. A new V1
  declaration is accepted only after metadata/checksum equality with that
  artifact; run success and injected-failure opens on iOS and macOS.
- [ ] Add an App-Group advisory bootstrap lock around every on-disk
  `ModelContainer` open/migration in app, widget, and intent processes, before
  SwiftData leases are readable. It is bounded/cancellable and complements the
  process lock. A two-process pause test proves no partial migration or reset.
- [ ] Realm selection is a stable realm-keyed map. Migrate the legacy selected
  child into `.local`, rewrite/copy it in the realm transition, reject stale
  widget selection, and purge only the deleted shared realm. Test same UUID in
  local/shared and server switching.
- [ ] Settings presentation contracts and non-transport value types live in a
  dependency-neutral target (or Settings-owned view state with app adapters).
  `SettingsFeature` remains independent of `BabySync`; neither target imports
  the other in reverse. Add a package dependency-graph build test.
- [ ] Every task that adds package tests updates and stages `EXPECTED_TESTS` in
  that same commit; no later task repairs a stale literal count.

### P3.2 store-owned writes and child cascade

- [ ] Replace caller mutation of SwiftData objects with store-owned patch/value
  commands. Every screen/model captures `RealmWriteContext` when rendered and
  supplies it on edit/delete; no optional default may recapture a newer realm.
  Include History, baby edit, logging sheets, Home, widgets, and Live Activity
  intents. A rejected stale write leaves persisted and in-memory values
  unchanged.
- [ ] Shared child delete creates one frozen baby-delete mutation and a derived,
  reversible local cascade overlay over child, dependent records, timers,
  selected child, widgets, and activities. Owner success reconciles the ordered
  server cascade; rejection/stale role restores the overlay deterministically.
  Caregiver, offline, rollback, server cascade, and distinct local-only
  behavior have tests.
- [ ] Run all existing BabyStore, History, Logging, Home, widget, and Live
  Activity tests in this task because it changes every write boundary.

### P3.3 endpoint, Keychain, invitation, Apple state, and cleanup

- [ ] Parsing link/QR/paste yields side-effect-free
  `InvitationIntent{canonicalOrigin,token}`, not a confirmed endpoint. Then
  probe `/v1/server` unauthenticated, confirm custom/changed instance, perform
  Apple authentication, fetch a safe preview, show final join confirmation,
  and redeem. Duplicate fragment `token` parameters reject. One
  `createInvitation()` response supplies the secret used to derive both share
  URL and QR; neither path refetches or creates another invitation.
- [ ] Use `ServerInfo.apple_authorization_mode`: send Apple authorization code
  only for `code_exchange_required`; omit it for `identity_token_only`. Bind
  installed credentials to the `SessionBundle.recovery_epoch`, not an earlier
  server probe. A probe/exchange epoch race never installs under the stale key.
- [ ] Persist Apple's opaque credential-state user ID only in Keychain under
  canonical origin, instance, account, and device. It is never sent to the
  server or stored in SwiftData. `.authorized` retains access; `.revoked` and
  `.notFound` start durable cleanup; `.transferred` surfaces migration-required
  without deleting server data; provider errors and temporarily protected
  Keychain retain state and retry.
- [ ] Replace endpoint-only continuity lookup with
  `ContinuityKey(canonicalOrigin,serverInstanceID,accountID,deviceID)`. A new
  Apple user cannot borrow another account's device. Test two accounts on one
  instance and two devices on one account.
- [ ] Pin Keychain service/account encoding,
  `kSecAttrSynchronizable=false`, duplicate/update/delete behavior, error
  mapping, and a this-device-only accessibility class compatible with selected
  background/widget access. Add the exact access-group entitlement to app and
  widget and verify effective entitlements on a signed build. A protected
  Keychain produces locked/unavailable UI, never purge.
- [ ] Add a non-secret idempotent cleanup checkpoint. First durably deny/hide
  the shared realm, then stop transport, erase the exact session/continuity per
  lifecycle policy, purge rows/selection, end activities, and reload widgets.
  Explicit logout/account/family delete stores its command ID before send;
  after any possibly-sent outcome it reconciles instead of blindly resending
  with erased credentials. Crash/relaunch tests cover every durable boundary.

### P3.4 import and realm activation

- [ ] Membership promotion rekeys the Keychain item and records
  `membershipReady` only; it does not bind/expose the shared realm. The local
  realm remains active through frozen manifest, complete mapping, ID rewrite,
  selection/outbox move, and canonical baby establishment. One fenced P3.4
  commit activates shared. Relaunch never repeats membership or exposes an
  empty shared realm.
- [ ] `FrozenImportManifest.sourceBases` is keyed by
  `EntityKey(kind,id)`, and preflight rejects legacy cross-kind UUID collisions
  with a repair/export conflict rather than overwriting a base.
- [ ] Resolver-created babies are never emitted again as client creates. The
  resolver returns their canonical entity/cursor or the client completes a
  fixed snapshot before activation. `import_expired` allocates a new import ID
  over the same frozen bytes/mode; it never refreezes live edits. Post-freeze
  writes remain ordered overlays. Test lost response, expiry, relaunch before
  first snapshot, and resolver-normalized presentation differences.
- [ ] A finalized ambiguous import still expires while `mapping_complete` is
  false. On `import_expired`, preserve the old ID only as terminal local history
  and allocate a new ID over the same frozen manifest bytes. Surface
  nonretryable `import_history_limit` without rotating IDs in a loop; an
  existing checkpoint may still status/retry when the server retained-ID cap is
  reached.
- [ ] Conflict pages are revision-bound and drained from page one through
  `conflicts_complete`; a stale page token restarts paging without losing the
  frozen manifest, mapping checkpoint, or ordered post-import intents. The UI
  never labels a truncated first page as the complete conflict set.

### P3.5 sync, epoch, faults, and client actions

- [ ] Freeze this exact wire-code action table:

  | Wire code/class | Required action |
  | --- | --- |
  | `membership_revoked`, family unavailable/recovery-only | Durable deny, stop, purge exact shared realm, activate local. |
  | `access_expired` | One fenced refresh, then exact request retry. |
  | `session_revoked`, `refresh_reused`, `reauthentication_required` | Preserve frozen intents; require fresh Apple proof and qualified continuity. |
  | `epoch_changed` | Enter internal `recoveryEpochChanged`, stop incremental sync, reauthenticate, checkpointed Keychain rekey, fixed snapshot. No second wire alias exists. |
  | `entitlement_required` | Pause/surface entitlement; never purge. |
  | Quarantine, retryable `5xx`, DB/network timeout | Retain and bounded retry. |
  | Permanent mutation rejection | Quarantine only that mutation. |

- [ ] `SecureSessionStore` performs a checkpointed old-epoch→new-epoch replace
  with at most one usable bearer item; continuity remains separate. Cold launch
  reconciles active realm, account gate, promotion/cleanup/rekey checkpoints,
  and Keychain before shared queries. Missing credentials fail closed into
  resumable cleanup; protected Keychain shows unavailable.
- [ ] Account and family session keys both include the minting recovery epoch
  carried in `SessionCredentials`. Epoch recovery uses checkpointed
  `rekeyAccount` before family membership and `rekeyFamily` after membership.
  `AccountSessionKey.leaseKey` intentionally projects only origin, instance,
  account, and device, preserving the same four-field refresh-serialization
  identity across epoch rekey.
- [ ] Adapt to advertised effective item and encoded-byte limits before freezing
  chunks. Route capability requirements follow P2's matrix. Actor labels page at
  a fixed high-water and retain `Former caregiver` after active membership
  removal.
- [ ] Wire-fault tests cover additive fields, unknown optional/required kinds,
  malformed/overlarge/duplicate JSON, wrong instance/epoch/family, outcome
  count/name mismatch, repeated/reordered pages, expired/forged page tokens,
  429 `Retry-After`, committed-prefix 503, wrong socket subprotocol, oversized
  frames, and stale hints. Every rejection preserves cursor and frozen outbox.
- [ ] Terminate/relaunch after every durable boundary of promotion, epoch rekey,
  import, snapshot, revocation, disconnect, and deletion; assert realm, gate,
  Keychain item count, outbox, page/cursor, selection, activities, and widgets.

### P3.6-P3.7 background, QR, widget, and honest E2E evidence

- [ ] Define one production BG app-refresh identifier, add it to permitted Info
  identifiers, register before launch completion, reschedule after foreground/
  success, consult account gate, and cancel/release leases on expiration. Mock
  scheduler tests exercise every completion path. This is the self-host
  best-effort background path and is independent of APNs.
- [ ] Add camera usage description and permission adapter with authorized,
  denied, restricted, and unavailable states; manual paste is always present.
  Simulator automation may paste, but signed-device acceptance scans the exact
  link's QR.
- [ ] Choose a complete widget contract: link `BabySync` plus shared Keychain
  entitlement and bounded request composition, or keep widgets local-read-only
  and leave network sync to app/BG task. The task must not ship a half-wired
  privileged extension. Preserve the rendered foreground widget-to-app proof.
- [ ] Local HTTPS E2E uses a trusted simulator CA or controlled HTTPS reverse
  proxy. Release tests reject HTTP, redirects, and debug trust hooks. A
  compile-time-only external control listener exchanges opaque fixture handles
  for test proofs; Release app/server contain no route, issuer key, or debug
  origin, and no secret is passed in launch arguments.
- [ ] Automated protocol evidence uses two independent helper processes with
  separate store roots and test Keychain namespaces against the reported real
  container. Rendered UI uses one installed app plus externally committed
  changes. “Two installations” is reserved for two simulator UDIDs/app
  containers where supported and two signed physical devices before beta;
  evidence records device IDs, app SHA, and server image digest without secrets.
- [ ] Run the genuine two-client wire-fault/cold-launch suite and record external
  blockers separately for real Apple, Associated Domains/AASA, signed Keychain
  group, camera QR, BG scheduling, Live Activities, and two devices. Fake issuer
  success never marks those public-beta gates complete.

## Whole-plan Acceptance Gate

- [ ] Run `rtk make test` and confirm the updated literal test count matches every Swift package summary.
- [ ] Run `rtk make build` and `rtk make test-ui` for the complete `BabyTrackerUITests` suite on the iOS simulator.
- [ ] Run the copied V1 App Group store migration on an iOS simulator/device and prove the failure path leaves the original store intact rather than resetting it.
- [ ] Run `rtk make test-family-sync-e2e` against the production Go container and PostgreSQL with its compile-time-only test issuer: create/join, overlapping import, all record types, M1/M2 delayed response, fixed-high-water pages, uncertain timer/collision/dependent stop, lost start response then token expiry/fresh Apple proof/old-device receipt, revocation purge, import interruption with frozen manifest chunks/post-import edits, token refresh serialization and uncertain-takeover fresh proof with no refresh-token reuse, and recovery-epoch snapshot.
- [ ] Manually verify real Sign in with Apple, hosted Universal Link, Keychain access group, foreground widget-to-app refresh, Live Activity update, and two physical/signed installations after the hosted signing/domain inputs are approved. Simulator and fake-issuer results do not establish those release claims.

## Self-Review

- **Spec coverage:** P3.1 covers predecessor-built store migration, date-only values, local realms, protocol fixtures, process-safe transition/refresh fencing, and migration-plan wiring; P3.2 covers transactional outbox, persisted frozen dispatch, and all `already_applied.original_status` timer results; P3.3 covers D6, pre-family/account/family Keychain rekey, P1 device continuity, credential state, Apple/name/profile/member/owner/delete/recover/forfeit/QR lifecycle, invitation confirmation, and D10; P3.4 covers frozen-manifest exact import plus ordered post-import offline intents; P3.5 covers cursor/base-overlay/retry/old-device receipt recovery/recovery epoch/revocation; P3.6 consumes the lease for realtime/rendered updates and owns the reusable UI/E2E test-server driver; P3.7 owns automated two-client evidence and the separate signed-installation acceptance record. No iOS-owned requirement is unassigned.
- **Completeness scan:** Every task has concrete files, tests, fixture intent, commands, implementation signatures, ownership-safe staging, and a conventional commit; no deferred or generic implementation shorthand remains.
- **Type consistency:** `RealmID`, `AccountLeaseKey`, `RealmWriteContext`, `RefreshAttempt`, `RefreshAttemptState`, `LeaseScope`, `FencedLeaseToken`, `FencedLeaseStore`, `WireMutation`, `SyncResponse`, `SnapshotResponse`, `SyncPersistence`, `SyncCoordinator`, `SyncReason`, `AccountSessionKey`, `FamilySessionKey`, `DeviceContinuity`, `SecureSessionStore`, `FrozenImportManifest`, `ImportCheckpoint`, `SyncIssue`, `PersistedDispatch`, and timer delivery states are defined before later tasks consume them. P1 owns device-continuity verification and P2 owns wire JSON/endpoints; this plan decodes those contracts rather than redefining them.
- **Review Focus:** Each of the five listed cross-plan risks is pinned to a named focused client test: stable revocation after authorization denial, cursor high-water, timer receipt-loss, realm contention, and recovery-epoch non-reupload. D6 and D10 also have their named P3.3 tests and explicit constraints.

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-09-20-family-sync-ios-client.md`. Please review the plan. Does it capture what you want?
