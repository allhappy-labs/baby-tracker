import BabyCore
import BabyProtocol
import BabyStore
@testable import BabySync
import Foundation
import Testing
@testable import SettingsFeature

/// The private network, unreachable: every request fails as a phone off the
/// tailnet sees it.
private final class UnreachableNetwork: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var paths: [String] = []
    private static let lock = NSLock()
    static var requested: [String] { lock.withLock { paths } }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        Self.lock.withLock { Self.paths.append(request.url?.path ?? "") }
        client?.urlProtocol(self, didFailWithError: URLError(.notConnectedToInternet))
    }
    override func stopLoading() {}
}

/// The private network, answering: every request gets the one scripted body.
private final class AnsweringNetwork: URLProtocol, @unchecked Sendable {
    nonisolated(unsafe) static var body = Data()
    nonisolated(unsafe) static var paths: [String] = []
    private static let lock = NSLock()
    static var requested: [String] { lock.withLock { paths } }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        let body = Self.lock.withLock { () -> Data in
            Self.paths.append(request.url?.path ?? "")
            return Self.body
        }
        guard let url = request.url,
              let response = HTTPURLResponse(url: url, statusCode: 200, httpVersion: nil, headerFields: ["Content-Type": "application/json"]) else {
            client?.urlProtocol(self, didFailWithError: URLError(.badServerResponse))
            return
        }
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: body)
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

/// The graph an App Intent builds for one Lock Screen action, in the process
/// it runs in: the App Group store the intent opened, the shared Keychain,
/// the saved family session, its own transport — and nothing left running
/// once the action returns.
@MainActor
@Suite(.serialized, .timeLimit(.minutes(1)))
struct LiveActivityBackgroundCompositionTests {
    static let instance = UUID(uuidString: "a8a8a8a8-a8a8-4a8a-8a8a-a8a8a8a8a8a8")!
    static let family = UUID(uuidString: "b8b8b8b8-b8b8-4b8b-8b8b-b8b8b8b8b8b8")!
    static let baby = UUID(uuidString: "c8c8c8c8-c8c8-4c8c-8c8c-c8c8c8c8c8c8")!

    struct Phone {
        let store: BabyStore
        let backend: InMemoryKeychainBackend
        let record: FamilySessionRecord
        let timer: ActiveTimer
        let target: LiveActivityActionTarget
    }

    /// A phone in the family whose last sync applied cursor 10, with a nap
    /// running and the server's card for it on the Lock Screen, built at
    /// `cardSequence` — past 10, the card is ahead of the store.
    static func phone(cardSequence: Int64 = 10) throws -> Phone {
        let origin = try ServerOrigin(validating: "https://sync.example.test")
        let key = FamilySessionKey(canonicalOrigin: origin.canonicalString, serverInstanceID: instance, familyID: family,
                                   accountID: UUID(), deviceID: UUID(), recoveryEpoch: 1)
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(Date(timeIntervalSince1970: 1_800_000_000)))
        store.context.insert(Baby(id: baby, name: "Mira", birthDate: .now, realm: key.realmID))
        try store.context.save()
        try store.setActiveRealm(key.realmID)
        try store.markAccountAuthenticated(key.leaseKey, credentialVersion: UUID())
        try store.applySyncPage(SyncResponse(serverInstanceID: instance, recoveryEpoch: 1, outcomes: [], changes: [],
                                             highWater: Cursor(serverInstanceID: instance, recoveryEpoch: 1, familyID: family, sequence: 10),
                                             nextPageToken: nil))
        let timer = try store.startSleep(for: baby)
        let backend = InMemoryKeychainBackend()
        try KeychainSessionStore(backend: backend).saveFamily(
            SessionCredentials(accessToken: "access", refreshToken: "refresh", expiresAt: .distantFuture, recoveryEpoch: 1,
                               deviceContinuity: DeviceContinuity(deviceID: key.deviceID, credential: "continuity")),
            for: key
        )
        let scope = try LiveActivityScope(serverInstanceID: instance, recoveryEpoch: 1, familyID: family, babyID: baby, kindRaw: "sleep")
        let revision = try LiveActivityRevision(scope: scope, generationID: UUID(), timerID: timer.id, familySequence: cardSequence,
                                                presentationVersion: 1, presentationTimestamp: 1_800_000_000, canWrite: true)
        return Phone(store: store, backend: backend,
                     record: FamilySessionRecord(endpoint: ConfirmedEndpoint(origin: origin, serverInstanceID: instance), familyKey: key),
                     timer: timer, target: LiveActivityActionTarget(revision: revision))
    }

    static var unreachable: URLSessionConfiguration {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [UnreachableNetwork.self]
        return configuration
    }

    /// A cold launch off the private network, on a card the store already
    /// vouches for: ready at once, without a single request.
    @Test func aCurrentCardIsReadyOffThePrivateNetworkWithoutARequest() async throws {
        let phone = try Self.phone()
        let composition = LiveActivityBackgroundComposition(
            store: phone.store, keychainBackend: phone.backend, record: phone.record, configuration: Self.unreachable
        )
        defer { composition.finish() }
        let before = UnreachableNetwork.requested.count

        #expect(await composition.prepare(target: phone.target, timeout: .seconds(30)) == .ready(try phone.store.captureWriteContext()))
        #expect(UnreachableNetwork.requested.count == before)
    }

    /// A cold launch off the private network, on a card ahead of the store:
    /// the real transport tries, fails, and the action is handed to the app.
    /// Once finished, the graph sends nothing more.
    @Test func aColdLaunchOffThePrivateNetworkHandsOffAndThenStops() async throws {
        let phone = try Self.phone(cardSequence: 11)
        let composition = LiveActivityBackgroundComposition(
            store: phone.store, keychainBackend: phone.backend, record: phone.record, configuration: Self.unreachable
        )
        let before = UnreachableNetwork.requested.count
        let outbox = Set(try phone.store.liveOutboxRows().map(\.id))

        #expect(await composition.prepare(target: phone.target, timeout: .seconds(30)) == .openApp(reason: .refreshFailed))
        #expect(UnreachableNetwork.requested.dropFirst(before).contains("/v1/sync"))
        #expect(try phone.store.activeTimer(kind: .sleep, for: Self.baby)?.id == phone.timer.id)
        #expect(Set(try phone.store.liveOutboxRows().map(\.id)) == outbox)

        composition.finish()
        let afterFinish = UnreachableNetwork.requested.count
        #expect(await composition.prepare(target: phone.target, timeout: .seconds(30)) == .openApp(reason: .refreshFailed))
        await composition.publish(timeout: .seconds(30))
        #expect(UnreachableNetwork.requested.count == afterFinish)
    }

    /// The private network reachable, and the card ahead of the store: the
    /// real transport's catch-up gets through, the store applies the cursor
    /// the server holds, and the action is ready in the checked realm. The
    /// publish afterwards goes the same way.
    @Test func aReachableFamilyServerMakesThePushedCardReady() async throws {
        let phone = try Self.phone(cardSequence: 11)
        AnsweringNetwork.body = try JSONEncoder().encode(SyncResponse(
            serverInstanceID: Self.instance, recoveryEpoch: 1, outcomes: [], changes: [],
            highWater: Cursor(serverInstanceID: Self.instance, recoveryEpoch: 1, familyID: Self.family, sequence: 11), nextPageToken: nil
        ))
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [AnsweringNetwork.self]
        let composition = LiveActivityBackgroundComposition(
            store: phone.store, keychainBackend: phone.backend, record: phone.record, configuration: configuration
        )
        defer { composition.finish() }
        let before = AnsweringNetwork.requested.count

        let decision = await composition.prepare(target: phone.target, timeout: .seconds(30))

        #expect(decision == .ready(try phone.store.captureWriteContext()))
        #expect(Array(AnsweringNetwork.requested.dropFirst(before)) == ["/v1/sync"])
        #expect(try phone.store.syncCursor()?.sequence == 11)
        #expect(try phone.store.activeTimer(kind: .sleep, for: Self.baby)?.id == phone.timer.id)

        await composition.publish(timeout: .seconds(30))
        #expect(Array(AnsweringNetwork.requested.dropFirst(before)) == ["/v1/sync", "/v1/sync"])
    }

    /// The renewer's identity client has a session of its own. It closes
    /// with the graph: nothing a renewal might have opened outlives the
    /// intent, and the URLSession actually reports itself invalid.
    @Test func finishingClosesTheIdentitySessionToo() async throws {
        let phone = try Self.phone()
        let composition = LiveActivityBackgroundComposition(
            store: phone.store, keychainBackend: phone.backend, record: phone.record,
            configuration: Self.unreachable, identityConfiguration: Self.unreachable
        )
        let identity = try #require(composition.identity)
        let (invalidations, continuation) = AsyncStream.makeStream(of: Void.self)
        identity.onSessionInvalidated = {
            continuation.yield(())
            continuation.finish()
        }
        #expect(!identity.isInvalidated)

        composition.finish()

        #expect(identity.isInvalidated)
        guard identity.isInvalidated else { return }
        var iterator = invalidations.makeAsyncIterator()
        #expect(await iterator.next() != nil)
    }

    /// No saved family session — signed out, or a session record that never
    /// reached this process — is no way to vouch for a pushed card ahead of
    /// the store.
    @Test func withoutASavedSessionAPushedCardHandsOff() async throws {
        let phone = try Self.phone(cardSequence: 11)
        let composition = LiveActivityBackgroundComposition(
            store: phone.store, keychainBackend: phone.backend, record: nil, configuration: Self.unreachable
        )
        defer { composition.finish() }

        #expect(await composition.prepare(target: phone.target, timeout: .seconds(30)) == .openApp(reason: .refreshFailed))
        #expect(try phone.store.activeTimer(kind: .sleep, for: Self.baby)?.id == phone.timer.id)
    }

    /// The process an intent runs in gets the provisional push state from the
    /// ledger before it touches a card — unless the app's own coordinator
    /// already owns it there, which must not be replaced.
    @Test func theProvisionalPushStateIsInstalledOnlyWhereNoCoordinatorOwnsIt() throws {
        let directory = FileManager.default.temporaryDirectory.appending(path: "LiveActivityBackground-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: directory) }
        let ledger = LiveActivityRegistrationStore(directory: directory, keychain: InMemoryKeychainBackend())

        let fresh = LiveActivityPushState()
        #expect(!fresh.hasRetireHandler)
        LiveActivityBackgroundComposition.installProvisionalPushState(into: fresh, from: ledger)
        #expect(fresh.hasRetireHandler)

        let owned = LiveActivityPushState()
        owned.setRetireHandler { _ in false }
        LiveActivityBackgroundComposition.installProvisionalPushState(into: owned, from: ledger)
        #expect(owned.retire(["card"]) == false)
    }
}
