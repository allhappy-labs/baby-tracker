import BabyCore
import BabyProtocol
import BabyStore
import Foundation
import Testing
@testable import BabySync

/// A scripted server. It records every request, which is how these tests prove
/// the negatives — that a signed-out account sends nothing at all.
@MainActor
final class FakeSyncTransport: FamilySyncTransport, @unchecked Sendable {
    var pages: [SyncResponse] = []
    var snapshotPages: [SnapshotResponse] = []
    var failWith: SyncErrorCode?
    var boundFailure: CredentialBoundAuthFailure?
    private(set) var requests: [SyncRequest] = []
    private(set) var snapshotRequests = 0

    func sync(_ request: SyncRequest) async throws -> SyncResponse {
        requests.append(request)
        if let boundFailure { throw boundFailure }
        if let failWith {
            self.failWith = nil
            throw failWith
        }
        guard !pages.isEmpty else { return SyncFixtures.page() }
        return pages.removeFirst()
    }

    func snapshot(pageToken: String?) async throws -> SnapshotResponse {
        snapshotRequests += 1
        guard !snapshotPages.isEmpty else { return SyncFixtures.snapshot() }
        return snapshotPages.removeFirst()
    }
}

/// The server's `invalid_request`: the whole request refused at decode
/// (`Server/internal/httpapi/sync.go`) because one entry in it is malformed.
@MainActor
final class RefusingTransport: FamilySyncTransport, @unchecked Sendable {
    let inner: any FamilySyncTransport
    let refusedEntityID: UUID

    init(inner: any FamilySyncTransport, refusedEntityID: UUID) {
        self.inner = inner
        self.refusedEntityID = refusedEntityID
    }

    func sync(_ request: SyncRequest) async throws -> SyncResponse {
        if request.mutations.contains(where: { $0.entityID == refusedEntityID }) { throw SyncErrorCode.invalidRequest }
        return try await inner.sync(request)
    }

    func snapshot(pageToken: String?) async throws -> SnapshotResponse { try await inner.snapshot(pageToken: pageToken) }
}

@MainActor
final class RefusingEverySync: FamilySyncTransport, @unchecked Sendable {
    let inner: FakeSyncTransport
    let code: SyncErrorCode
    private(set) var syncCalls = 0

    init(inner: FakeSyncTransport, code: SyncErrorCode) {
        self.inner = inner
        self.code = code
    }

    func sync(_ request: SyncRequest) async throws -> SyncResponse {
        syncCalls += 1
        throw code
    }

    func snapshot(pageToken: String?) async throws -> SnapshotResponse { try await inner.snapshot(pageToken: pageToken) }
}

enum SyncFixtures {
    static let instanceID = UUID(uuidString: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")!
    static let familyID = UUID(uuidString: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")!
    static let accountID = UUID(uuidString: "dddddddd-dddd-4ddd-8ddd-dddddddddddd")!
    static let deviceID = UUID(uuidString: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee")!
    static let babyID = UUID(uuidString: "10000000-0000-4000-8000-000000000001")!

    static var realm: RealmID {
        .shared(canonicalOrigin: "https://sync.example", serverInstanceID: instanceID, familyID: familyID)
    }

    static var accountKey: AccountSessionKey {
        AccountSessionKey(
            canonicalOrigin: "https://sync.example", serverInstanceID: instanceID,
            accountID: accountID, deviceID: deviceID, recoveryEpoch: 0
        )
    }

    static func cursor(_ sequence: Int64, epoch: Int64 = 1) -> Cursor {
        Cursor(serverInstanceID: instanceID, recoveryEpoch: epoch, familyID: familyID, sequence: sequence)
    }

    static func page(nextPageToken: String? = nil, highWater: Cursor = cursor(1)) -> SyncResponse {
        SyncResponse(
            serverInstanceID: instanceID, recoveryEpoch: highWater.recoveryEpoch,
            outcomes: [], changes: [], highWater: highWater, nextPageToken: nextPageToken
        )
    }

    static func snapshot(nextPageToken: String? = nil, snapshotID: UUID = UUID()) -> SnapshotResponse {
        SnapshotResponse(
            serverInstanceID: instanceID, recoveryEpoch: 2, snapshotID: snapshotID,
            highWater: cursor(5, epoch: 2), entities: [], nextPageToken: nextPageToken
        )
    }

    @MainActor
    static func store() throws -> BabyStore {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        let baby = Baby(id: babyID, name: "Mira", birthDate: .now, realm: realm)
        store.context.insert(baby)
        try store.context.save()
        try store.setActiveRealm(realm)
        try store.markAccountAuthenticated(accountKey.leaseKey, credentialVersion: UUID())
        return store
    }
}

@MainActor
@Suite("Sync coordinator")
struct SyncCoordinatorTests {
    private func make(store: BabyStore? = nil) throws -> (BabyStore, FakeSyncTransport, SyncCoordinator) {
        let store = try store ?? SyncFixtures.store()
        let transport = FakeSyncTransport()
        let coordinator = SyncCoordinator(
            store: store, transport: transport, accountKey: SyncFixtures.accountKey
        )
        return (store, transport, coordinator)
    }

    @Test func obsoleteCredentialFailureCannotInvalidateFreshSignInOrClearTerminalIssue() async throws {
        let store = try SyncFixtures.store()
        let transport = FakeSyncTransport()
        let parentVersion = UUID()
        try store.markAccountAuthenticated(SyncFixtures.accountKey.leaseKey, credentialVersion: parentVersion)
        var freshProofs = 0
        let coordinator = SyncCoordinator(store: store, transport: transport, accountKey: SyncFixtures.accountKey, onFreshProofRequired: { freshProofs += 1 })
        transport.boundFailure = CredentialBoundAuthFailure(underlying: .status(401, "session_revoked"), credentialVersion: parentVersion)
        await coordinator.syncOnce(reason: .foreground)
        #expect(freshProofs == 1)
        let terminal = coordinator.currentIssue
        #expect(terminal != nil)
        try store.markAccountSignedIn(SyncFixtures.accountKey.leaseKey, credentialVersion: UUID())
        await coordinator.syncOnce(reason: .foreground)
        #expect(freshProofs == 1)
        #expect(coordinator.currentIssue == terminal)
        #expect(try store.accountAllowsOrdinarySync(SyncFixtures.accountKey.leaseKey))
    }

    @Test func compositeFeedOutboxUsesOrderedPayloadV4() throws {
        let (store, _, _) = try make()
        let feed = try store.logBottle(volumeML: 1, contents: .formula, for: SyncFixtures.babyID)
        feed.components = [
            .bottle(id: UUID(uuidString: "30000000-0000-4000-8000-000000000001")!, volumeML: 50, contents: .breastMilk),
            .bottle(id: UUID(uuidString: "30000000-0000-4000-8000-000000000002")!, volumeML: 30, contents: .formula),
        ]
        try store.update(feed)

        let mutation = try #require(try store.pendingOutbox().last).decodedMutation()
        #expect(mutation.payloadVersion == 4)
        guard case .object(let payload) = mutation.payload,
              case .array(let components) = payload["components"] else {
            Issue.record("expected ordered components in feed payload")
            return
        }
        #expect(components.count == 2)
        guard case .object(let first) = components[0], case .object(let second) = components[1] else {
            Issue.record("expected component objects")
            return
        }
        #expect(first["contents"] == .string("breast_milk"))
        #expect(second["contents"] == .string("formula"))
    }

    @Test func timedAndUntimedBottleFeedsUseV4() throws {
        let (store, _, _) = try make()
        let feed = try store.logBottle(volumeML: 60, contents: .formula, for: SyncFixtures.babyID)
        let untimed = try #require(try store.pendingOutbox().last).decodedMutation()
        #expect(untimed.payloadVersion == 4)
        feed.components = [.bottle(id: UUID(), volumeML: 60, contents: .formula, seconds: 135)]
        try store.update(feed)
        let timed = try #require(try store.pendingOutbox().last).decodedMutation()
        #expect(timed.payloadVersion == 4)
        guard case let .object(payload) = timed.payload,
              case let .array(components) = payload["components"],
              case let .object(bottle) = components.first else {
            Issue.record("expected bottle component in timed feed payload")
            return
        }
        #expect(bottle["seconds"] == .number(135))
    }

    /// The server requires the header set to be EXACTLY the route's set
    /// (`ValidateRouteCapabilities`, `Server/internal/protocol/sync.go`):
    /// `records-v1` for a sync, plus `timer-commands-v1` when the batch holds
    /// a timer mutation. `live-hints-v1` belongs to `/v1/live` only. Sending
    /// it here made every sync a 409 `unsupported_capability` — found by the
    /// two-caregiver live-server gate.
    @Test func everyRequestAdvertisesTheExactCapabilities() async throws {
        let (_, transport, coordinator) = try make()
        await coordinator.syncOnce(reason: .launch)

        #expect(transport.requests.first?.capabilities == ["records-v1"])
        #expect(transport.requests.first?.protocolVersion == "v1")
    }

    @Test func aBatchWithATimerAdvertisesTimerCommands() async throws {
        let (store, transport, coordinator) = try make()
        _ = try store.startSleep(for: SyncFixtures.babyID)
        await coordinator.syncOnce(reason: .localWrite)

        let request = try #require(transport.requests.first)
        #expect(request.mutations.contains { $0.entityKind == .timer })
        #expect(request.capabilities == ["records-v1", "timer-commands-v1"])
    }

    /// Fix round 1. An attempt that returns before sending must not inherit
    /// the previous attempt's success.
    @Test func attemptThatCannotRunClearsLastSuccess() async throws {
        let (store, _, coordinator) = try make()
        await coordinator.syncOnce(reason: .launch)
        #expect(coordinator.lastAttemptSucceeded)

        try store.markAccountSignedOut(SyncFixtures.accountKey.leaseKey)
        await coordinator.syncOnce(reason: .foreground)

        #expect(!coordinator.lastAttemptSucceeded)
    }

    /// The gate. A signed-out account sends nothing — not a single request —
    /// from any process.
    @Test func signedOutAccountSendsNothing() async throws {
        let (store, transport, coordinator) = try make()
        try store.markAccountSignedOut(SyncFixtures.accountKey.leaseKey)

        for reason in [SyncReason.launch, .foreground, .localWrite, .liveHint, .backgroundPush, .extensionRequest] {
            await coordinator.syncOnce(reason: reason)
        }

        #expect(transport.requests.isEmpty)
    }

    /// Local-only use never talks to a server.
    @Test func localRealmSendsNothing() async throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        try store.markAccountAuthenticated(SyncFixtures.accountKey.leaseKey, credentialVersion: UUID())
        let (_, transport, coordinator) = try make(store: store)

        await coordinator.syncOnce(reason: .launch)

        #expect(transport.requests.isEmpty)
    }

    /// Every page is applied before the cursor is believed, and mutations ride
    /// only on the first request — resending them per page would duplicate
    /// them.
    @Test func pagedResponsesDrainAndMutationsRideOnlyTheFirstPage() async throws {
        let (store, transport, coordinator) = try make()
        _ = try store.logDiaper(.wet, for: SyncFixtures.babyID)
        transport.pages = [
            SyncFixtures.page(nextPageToken: "page-2", highWater: SyncFixtures.cursor(9)),
            SyncFixtures.page(nextPageToken: nil, highWater: SyncFixtures.cursor(9)),
        ]

        await coordinator.syncOnce(reason: .localWrite)

        #expect(transport.requests.count == 2)
        #expect(transport.requests.first?.mutations.count == 1)
        #expect(transport.requests.last?.mutations.isEmpty == true)
        #expect(try store.syncCursor() == SyncFixtures.cursor(9))
    }

    @Test func reentrantSyncIsIgnored() async throws {
        let (_, transport, coordinator) = try make()
        async let first: Void = coordinator.syncOnce(reason: .launch)
        async let second: Void = coordinator.syncOnce(reason: .liveHint)
        _ = await (first, second)

        #expect(transport.requests.count <= 2)
    }

    // MARK: The action table, end to end

    /// A revoked membership signs the account out and takes the family's rows
    /// with it, keeping the user's own local realm.
    @Test func revokedMembershipPurgesAndSignsOut() async throws {
        let (store, transport, coordinator) = try make()
        transport.failWith = .membershipRevoked

        await coordinator.syncOnce(reason: .foreground)

        #expect(try store.babies(in: SyncFixtures.realm).isEmpty)
        #expect(!(try store.accountAllowsOrdinarySync(SyncFixtures.accountKey.leaseKey)))
        #expect(coordinator.currentIssue?.code == SyncErrorCode.membershipRevoked.rawValue)
        #expect(coordinator.currentIssue?.presentation == .revoked)
    }

    /// A meaningless cursor is replaced by a snapshot rather than retried.
    @Test(arguments: [false, true]) func epochChangeTakesASnapshot(credentialBound: Bool) async throws {
        let store = try SyncFixtures.store()
        let transport = FakeSyncTransport()
        var proofs = 0
        let coordinator = SyncCoordinator(store: store, transport: transport, accountKey: SyncFixtures.accountKey, onFreshProofRequired: { proofs += 1 })
        if credentialBound {
            let version = UUID()
            try store.markAccountAuthenticated(SyncFixtures.accountKey.leaseKey, credentialVersion: version)
            transport.boundFailure = CredentialBoundAuthFailure(underlying: .status(401, "epoch_changed"), credentialVersion: version)
        } else { transport.failWith = .epochChanged }
        let snapshotID = UUID()
        transport.snapshotPages = [SyncFixtures.snapshot(snapshotID: snapshotID)]
        // The snapshot is followed by one pull from its own high water; this
        // is the page that answers it, carrying the cursor forward unchanged
        // since nothing new has landed.
        transport.pages = [SyncFixtures.page(highWater: SyncFixtures.cursor(5, epoch: 2))]

        await coordinator.syncOnce(reason: .foreground)

        #expect(transport.snapshotRequests == 1)
        #expect(transport.requests.count == 2)
        #expect(transport.requests.last?.cursor == SyncFixtures.cursor(5, epoch: 2))
        #expect(try store.syncCursor() == SyncFixtures.cursor(5, epoch: 2))
        #expect(coordinator.currentIssue == nil)
        #expect(proofs == 0)
    }

    /// A snapshot pages current rows and is not frozen: a write that lands
    /// while it pages can be half visible. One pull from the snapshot's high
    /// water, straight after it, settles the device without waiting for the
    /// next poll.
    @Test func snapshotIsFollowedByOnePullFromItsHighWater() async throws {
        let (store, transport, coordinator) = try make()
        transport.failWith = .snapshotRequired
        transport.snapshotPages = [SyncFixtures.snapshot()] // high water 5, epoch 2
        transport.pages = [SyncFixtures.page(highWater: SyncFixtures.cursor(7, epoch: 2))]

        await coordinator.syncOnce(reason: .foreground)

        #expect(transport.snapshotRequests == 1)
        #expect(transport.requests.count == 2)
        #expect(transport.requests[1].cursor == SyncFixtures.cursor(5, epoch: 2))
        #expect(transport.requests[1].pageToken == nil)
        #expect(try store.syncCursor() == SyncFixtures.cursor(7, epoch: 2))
        #expect(coordinator.currentIssue == nil)
    }

    /// The pull after a snapshot is one attempt: a refusal is not answered
    /// with another snapshot from inside the first.
    @Test func aRefusedPullAfterASnapshotDoesNotRecurse() async throws {
        let inner = FakeSyncTransport()
        inner.snapshotPages = [SyncFixtures.snapshot()]
        let refusing = RefusingEverySync(inner: inner, code: .snapshotRequired)
        let coordinator = SyncCoordinator(store: try SyncFixtures.store(), transport: refusing, accountKey: SyncFixtures.accountKey)

        await coordinator.syncOnce(reason: .foreground)

        #expect(inner.snapshotRequests == 1)
        #expect(refusing.syncCalls == 2) // the attempt, then the one pull after the snapshot
    }

    /// A size complaint shrinks the batch and stays silent — it is the
    /// client's job to send less, not the parent's problem.
    @Test func batchLimitShrinksTheBatchWithoutSurfacing() async throws {
        let (_, transport, coordinator) = try make()
        let before = coordinator.batchLimit
        transport.failWith = .batchLimit

        await coordinator.syncOnce(reason: .localWrite)

        #expect(coordinator.batchLimit == before / 2)
        #expect(coordinator.currentIssue == nil)
    }

    /// `invalid_request` refuses the whole request at decode, not one entry
    /// in it, so the first refusal cannot say which entry is bad. It halves
    /// the batch and quarantines nothing yet.
    @Test func refusedBatchIsHalvedBeforeAnythingIsQuarantined() async throws {
        let (store, transport, coordinator) = try make()
        _ = try store.logDiaper(.wet, for: SyncFixtures.babyID)
        _ = try store.logDiaper(.dirty, for: SyncFixtures.babyID)
        let before = coordinator.batchLimit
        transport.failWith = .invalidRequest

        await coordinator.syncOnce(reason: .localWrite)

        #expect(try store.nextDispatchBatch(limit: 10).count == 2)
        #expect(coordinator.batchLimit == before / 2)
        #expect(coordinator.currentIssue == nil)
    }

    /// One hopeless mutation behind good ones: the good ones are applied, and
    /// only the hopeless one is quarantined — once it is alone in its batch,
    /// the only point at which the refusal provably names it.
    @Test func onlyTheRefusedEntryIsQuarantinedAndTheGoodOnesAreApplied() async throws {
        let family = try Family()
        let (baby, _) = try family.seedOwner()
        try await family.ownerEngine().activateAsOwner()
        let good = try (0..<3).map { _ in try family.owner.logDiaper(.wet, for: baby.id) }
        let bad = try family.owner.logDiaper(.dirty, for: baby.id)
        let coordinator = SyncCoordinator(
            store: family.owner,
            transport: RefusingTransport(inner: family.ownerTransport, refusedEntityID: bad.id),
            accountKey: family.ownerKey.accountSessionKey
        )

        var passes = 0
        while try !family.owner.nextDispatchBatch(limit: 10).isEmpty, passes < 30 {
            await coordinator.syncOnce(reason: .localWrite)
            passes += 1
        }

        #expect(family.server.receivedEntityIDs.isSuperset(of: good.map(\.id)))
        #expect(!family.server.receivedEntityIDs.contains(bad.id))
        let quarantined = try family.owner.liveOutboxRows(in: family.ownerKey.realmID)
            .filter { $0.deliveryState == .quarantined }
        #expect(quarantined.map(\.entityID) == [bad.id])
        // And the next batch is full-sized again, not stuck at one.
        #expect(coordinator.batchLimit == 100)
    }

    /// A read-only caregiver is told why, in words about sharing rather than
    /// about an error code.
    @Test func readOnlyCaregiverIsExplainedNotRetried() async throws {
        let (_, transport, coordinator) = try make()
        transport.failWith = .readOnlyCaregiver

        await coordinator.syncOnce(reason: .localWrite)

        #expect(coordinator.currentIssue?.code == SyncErrorCode.readOnlyCaregiver.rawValue)
        #expect(coordinator.currentIssue?.presentation.message.contains("view-only") == true)
    }

    @Test func expiredAccessAsksForARefreshBeforeSurfacing() async throws {
        let store = try SyncFixtures.store()
        let transport = FakeSyncTransport()
        var refreshes = 0
        let coordinator = SyncCoordinator(
            store: store, transport: transport, accountKey: SyncFixtures.accountKey,
            onRefreshRequired: { refreshes += 1 }
        )
        transport.failWith = .accessExpired

        await coordinator.syncOnce(reason: .foreground)

        #expect(refreshes == 1)
        #expect(coordinator.currentIssue == nil)
    }

    /// A renewal that could not run because another process holds the lease
    /// is a moment's wait, not a reason to ask the parent for Apple proof.
    @Test func transientRenewalFailureBacksOffWithoutAskingForSignIn() async throws {
        let store = try SyncFixtures.store()
        let transport = FakeSyncTransport()
        var freshProofs = 0
        let coordinator = SyncCoordinator(
            store: store, transport: transport, accountKey: SyncFixtures.accountKey,
            onFreshProofRequired: { freshProofs += 1 },
            onRefreshRequired: { throw SessionRefresher.RefreshError.lockUnavailable }
        )
        transport.failWith = .accessExpired

        await coordinator.syncOnce(reason: .foreground)

        #expect(freshProofs == 0)
        #expect(coordinator.currentIssue == nil)
        #expect(!coordinator.lastAttemptSucceeded)
    }

    /// A terminally invalid session has its refresh refused.
    /// That is the one renewal failure a sign-in fixes, so it asks for one.
    @Test func renewalRefusedForGoodAsksForFreshProof() async throws {
        let store = try SyncFixtures.store()
        let transport = FakeSyncTransport()
        var freshProofs = 0
        let coordinator = SyncCoordinator(
            store: store, transport: transport, accountKey: SyncFixtures.accountKey,
            onFreshProofRequired: { freshProofs += 1 },
            onRefreshRequired: { throw HTTPClientError.status(401, "reauthentication_required") }
        )
        transport.failWith = .accessExpired

        await coordinator.syncOnce(reason: .foreground)

        #expect(freshProofs == 1)
        #expect(coordinator.currentIssue?.code == SyncErrorCode.reauthenticationRequired.rawValue)
    }

    /// A revoked session cannot be refreshed; only Apple can vouch again.
    @Test func revokedSessionAsksForFreshProof() async throws {
        let store = try SyncFixtures.store()
        let transport = FakeSyncTransport()
        var freshProofs = 0
        let coordinator = SyncCoordinator(
            store: store, transport: transport, accountKey: SyncFixtures.accountKey,
            onFreshProofRequired: { freshProofs += 1 }
        )
        transport.failWith = .sessionRevoked

        await coordinator.syncOnce(reason: .foreground)

        #expect(freshProofs == 1)
        #expect(coordinator.currentIssue?.presentation.message.contains("Sign in with Apple") == true)
    }

    /// Being offline is reported as offline, not as a problem with the data.
    @Test func networkFailureReassuresRatherThanAlarms() async throws {
        struct Offline: Error {}
        let store = try SyncFixtures.store()
        final class FailingTransport: FamilySyncTransport, @unchecked Sendable {
            func sync(_ request: SyncRequest) async throws -> SyncResponse { throw Offline() }
            func snapshot(pageToken: String?) async throws -> SnapshotResponse { throw Offline() }
        }
        let coordinator = SyncCoordinator(
            store: store, transport: FailingTransport(), accountKey: SyncFixtures.accountKey
        )

        await coordinator.syncOnce(reason: .launch)

        // Offline is not an issue: nothing is wrong with the data and nothing
        // is asked of the user.
        #expect(coordinator.isOffline)
        #expect(coordinator.currentIssue == nil)
    }
}
