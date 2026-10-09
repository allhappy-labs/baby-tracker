import BabyProtocol
import BabyCore
import BabyStore
import Foundation
import Testing
@testable import BabySync

@MainActor
@Suite("Session refresh serialization")
struct SessionRefresherTests {
    private struct Harness {
        let store: BabyStore
        let keychain: KeychainSessionStore
        let leases: AppGroupFencedLease
        let directory: URL
    }

    private func harness() throws -> Harness {
        let directory = FileManager.default.temporaryDirectory
            .appending(path: "refresh-\(UUID().uuidString)", directoryHint: .isDirectory)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let container = try BabySchema.container(
            storeURL: directory.appending(path: "store.sqlite"),
            bootstrapLockURL: directory.appending(path: "bootstrap.lock")
        )
        let lockURL = directory.appending(path: "coordination.lock")
        let store = BabyStore(
            container: container, dateProvider: TestDateProvider(.now),
            deviceID: "refresh-test", coordinationLockURL: lockURL
        )
        return Harness(
            store: store,
            keychain: KeychainSessionStore(backend: InMemoryKeychainBackend()),
            leases: AppGroupFencedLease(container: container, lockURL: lockURL),
            directory: directory
        )
    }

    private func seed(_ harness: Harness) throws -> (AccountSessionKey, FamilySessionKey) {
        let accountKey = SessionFixture.accountKey()
        let familyKey = SessionFixture.familyKey()
        try harness.store.markAccountAuthenticated(accountKey.leaseKey, credentialVersion: SessionFixture.credentials().credentialVersion)
        try harness.keychain.saveFamily(SessionFixture.credentials(), for: familyKey)
        return (accountKey, familyKey)
    }

    @Test func successfulRefreshInstallsNewCredentialsAndClearsTheMarker() async throws {
        let harness = try harness()
        let (accountKey, familyKey) = try seed(harness)
        let refresher = SessionRefresher(
            store: harness.store, keychain: harness.keychain, leases: harness.leases
        ) { _ in SessionFixture.credentials(access: "new-access", refresh: "new-refresh") }

        try await refresher.refresh(accountKey: accountKey, familyKey: familyKey)

        #expect(try harness.keychain.loadFamily(familyKey)?.accessToken == "new-access")
        #expect(try harness.store.refreshAttemptState(for: accountKey.leaseKey) == .idle)
    }

    /// The marker is written before any byte leaves, so a failure mid-flight
    /// leaves it standing. A timeout or a lost reply means the refresh token
    /// may already be consumed.
    @Test func failedRefreshLeavesTheAttemptUncertain() async throws {
        struct Timeout: Error {}
        let harness = try harness()
        let (accountKey, familyKey) = try seed(harness)
        let refresher = SessionRefresher(
            store: harness.store, keychain: harness.keychain, leases: harness.leases
        ) { _ in throw Timeout() }

        await #expect(throws: (any Error).self) {
            try await refresher.refresh(accountKey: accountKey, familyKey: familyKey)
        }

        guard case .inFlightUncertain = try harness.store.refreshAttemptState(for: accountKey.leaseKey) else {
            Issue.record("a failed refresh must stay uncertain")
            return
        }
        // And the old credentials are untouched: nothing was installed.
        #expect(try harness.keychain.loadFamily(familyKey)?.accessToken == "access-token")
    }

    /// Reusing a possibly-consumed refresh token looks exactly like a stolen
    /// one and gets the session revoked. A standing marker forces fresh Apple
    /// proof instead.
    @Test func uncertainAttemptRefusesToRetryAndDemandsFreshProof() async throws {
        struct Timeout: Error {}
        let harness = try harness()
        let (accountKey, familyKey) = try seed(harness)
        var exchanges = 0
        let refresher = SessionRefresher(
            store: harness.store, keychain: harness.keychain, leases: harness.leases
        ) { _ in
            exchanges += 1
            throw Timeout()
        }
        try? await refresher.refresh(accountKey: accountKey, familyKey: familyKey)
        #expect(exchanges == 1)

        await #expect(throws: SessionRefresher.RefreshError.uncertainAttemptRequiresFreshProof) {
            try await refresher.refresh(accountKey: accountKey, familyKey: familyKey)
        }
        // The second attempt never reached the network.
        #expect(exchanges == 1)
    }

    /// Incident reproduction: an unexpired session's server rotation succeeds,
    /// but the reply is lost. Relaunching the client must have a recovery path
    /// without requiring Apple proof solely because the network dropped.
    /// The recovery protocol binds an exact replay to the same durable attempt
    /// and continuity proof; a bare legacy parent retry is never used.
    @Test func lostSuccessfulRefreshReplyDoesNotEndTheYearLongSignIn() async throws {
        let h = try RecoveryTestHarness(); try h.seed()
        let server = RecoveryTestTransport(lostReply: true)
        await #expect(throws: URLError.self) {
            try await h.refresher(server).refresh(accountKey: h.account, familyKey: h.family)
        }
        let pending = try #require(try h.store.refreshRecoveryCheckpoint(for: h.account.leaseKey))
        #expect(pending.phase == .awaitingReply)
        #expect(try h.store.accountAllowsOrdinarySync(h.account.leaseKey))
        let reopened = try RecoveryTestHarness(directory: h.directory)
        try await reopened.refresher(server).refresh(accountKey: h.account, familyKey: h.family)
        let requests = await server.requests
        #expect(requests.count == 2)
        #expect(requests[0].0 == requests[1].0)
        #expect(requests[0].1 == h.account.accountID)
        #expect(requests[0].2 == requests[1].2)
        #expect(requests[0].2 == pending.attempt.attemptID)
        #expect(await server.legacyRequests == 0)
        let installed = try #require(try reopened.keychain.loadFamily(h.family))
        #expect(installed.accessToken == "recovered-access")
        #expect(installed.refreshToken == "recovered-refresh")
        #expect(installed.installBinding?.attemptID == pending.attempt.attemptID)
        #expect(try reopened.store.refreshRecoveryCheckpoint(for: h.account.leaseKey)?.phase == .ackPending)
        #expect(try reopened.store.accountAllowsOrdinarySync(h.account.leaseKey))

        let mutations = FakeFamilyServer()
        LostReplyMutationProtocol.handle = { request, bytes in
            guard request.value(forHTTPHeaderField: "Authorization") == "Bearer recovered-access" else {
                return (401, Data("{\"error\":{\"code\":\"session_revoked\"}}".utf8))
            }
            let command = try JSONDecoder().decode(SyncRequest.self, from: bytes)
            let result = try mutations.sync(command, accountID: h.account.accountID, deviceID: h.account.deviceID)
            return (200, try JSONEncoder().encode(result))
        }
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [LostReplyMutationProtocol.self]
        let session = URLSession(configuration: configuration)
        defer { session.invalidateAndCancel(); LostReplyMutationProtocol.handle = nil }
        let family = h.family
        let client = FamilySyncHTTPClient(endpoint: SessionFixture.endpoint, tokens: KeychainTokenProvider(keychain: reopened.keychain, key: { .family(family) }), session: session)
        let command = WireMutation(mutationID: UUID(), entityID: UUID(), entityKind: .baby, operation: .create, payloadVersion: 1, observedEntityVersion: 0, clientMutationTime: "2026-10-01T00:00:00Z", importID: nil, payload: .object([:]))
        _ = try await client.sync(SyncRequest(protocolVersion: "v1", capabilities: ["records-v1"], cursor: nil, mutations: [command], pageToken: nil))
        #expect(mutations.applied.map(\.mutationID) == [command.mutationID])
    }

    /// Only a caller that can prove nothing was sent may clear the marker.
    @Test func provenUnsentAttemptCanBeCleared() async throws {
        struct NeverSent: Error {}
        let harness = try harness()
        let (accountKey, familyKey) = try seed(harness)
        let refresher = SessionRefresher(
            store: harness.store, keychain: harness.keychain, leases: harness.leases
        ) { _ in throw NeverSent() }
        try? await refresher.refresh(accountKey: accountKey, familyKey: familyKey)

        guard case .inFlightUncertain(let attempt) = try harness.store.refreshAttemptState(for: accountKey.leaseKey) else {
            Issue.record("expected an uncertain attempt")
            return
        }
        try refresher.abandonProvenUnsentAttempt(attempt.attemptID, accountKey: accountKey)

        #expect(try harness.store.refreshAttemptState(for: accountKey.leaseKey) == .idle)
    }

    /// A connection that never opened sent nothing, so the refresh token is
    /// provably unspent. Leaving the marker would turn every network blip —
    /// a phone in a lift, DNS still waking up — into a demand for Apple proof.
    @Test(arguments: [
        URLError.Code.notConnectedToInternet, .cannotConnectToHost, .cannotFindHost, .dnsLookupFailed,
    ])
    func failureBeforeAnyByteLeftClearsTheMarker(_ code: URLError.Code) async throws {
        let harness = try harness()
        let (accountKey, familyKey) = try seed(harness)
        var exchanges = 0
        let failing = SessionRefresher(
            store: harness.store, keychain: harness.keychain, leases: harness.leases
        ) { _ in
            exchanges += 1
            throw URLError(code)
        }

        await #expect(throws: URLError.self) {
            try await failing.refresh(accountKey: accountKey, familyKey: familyKey)
        }
        #expect(try harness.store.refreshAttemptState(for: accountKey.leaseKey) == .idle)

        // And the next attempt goes out with the same, still-unspent token.
        let working = SessionRefresher(
            store: harness.store, keychain: harness.keychain, leases: harness.leases
        ) { current in
            #expect(current.refreshToken == "refresh-token")
            return SessionFixture.credentials(access: "new-access", refresh: "new-refresh")
        }
        try await working.refresh(accountKey: accountKey, familyKey: familyKey)
        #expect(exchanges == 1)
        #expect(try harness.keychain.loadFamily(familyKey)?.accessToken == "new-access")
    }

    /// A timeout is the opposite case: the request may have arrived and the
    /// token been rotated, so the marker must stand.
    @Test func timeoutStaysUncertain() async throws {
        let harness = try harness()
        let (accountKey, familyKey) = try seed(harness)
        let refresher = SessionRefresher(
            store: harness.store, keychain: harness.keychain, leases: harness.leases
        ) { _ in throw URLError(.timedOut) }

        try? await refresher.refresh(accountKey: accountKey, familyKey: familyKey)

        guard case .inFlightUncertain = try harness.store.refreshAttemptState(for: accountKey.leaseKey) else {
            Issue.record("a timeout may have reached the server")
            return
        }
    }

    /// The refresh HTTP handler charges quotas before rotating the token.
    /// A definite rate-limit refusal therefore leaves the token unspent and
    /// must not turn a later retry into a demand for another Apple sign-in.
    @Test func explicitRateLimitRefusalAllowsRetryWithoutFreshAppleProof() async throws {
        let harness = try harness()
        let (accountKey, familyKey) = try seed(harness)
        var exchanges = 0
        let refresher = SessionRefresher(
            store: harness.store, keychain: harness.keychain, leases: harness.leases
        ) { current in
            exchanges += 1
            if exchanges == 1 { throw HTTPClientError.status(429, "rate_limited") }
            #expect(current.refreshToken == "refresh-token")
            return SessionFixture.credentials(access: "new-access", refresh: "new-refresh")
        }

        await #expect(throws: HTTPClientError.status(429, "rate_limited")) {
            try await refresher.refresh(accountKey: accountKey, familyKey: familyKey)
        }
        #expect(try harness.store.refreshAttemptState(for: accountKey.leaseKey) == .idle)
        #expect(try harness.store.accountAllowsOrdinarySync(accountKey.leaseKey))

        do {
            try await refresher.refresh(accountKey: accountKey, familyKey: familyKey)
        } catch {
            Issue.record("a rate-limited refresh must retry without fresh Apple proof: \(error)")
        }
        #expect(exchanges == 2)
        #expect(try harness.keychain.loadFamily(familyKey)?.accessToken == "new-access")
    }

    @Test(arguments: [
        HTTPClientError.status(429, nil), .status(429, "unknown_error"),
        .status(503, "service_unavailable"), .status(401, "session_revoked"),
    ])
    func otherHTTPFailuresRetainUncertainAttempt(_ error: HTTPClientError) async throws {
        let harness = try harness()
        let (accountKey, familyKey) = try seed(harness)
        let refresher = SessionRefresher(
            store: harness.store, keychain: harness.keychain, leases: harness.leases
        ) { _ in throw error }

        await #expect(throws: error) {
            try await refresher.refresh(accountKey: accountKey, familyKey: familyKey)
        }
        guard case .inFlightUncertain = try harness.store.refreshAttemptState(for: accountKey.leaseKey) else {
            Issue.record("an unspecified or terminal refusal must retain refresh uncertainty")
            return
        }
        await #expect(throws: SessionRefresher.RefreshError.uncertainAttemptRequiresFreshProof) {
            try await refresher.refresh(accountKey: accountKey, familyKey: familyKey)
        }
    }

    /// `engine.stop()` cancels the running sync. A renewal cancelled before
    /// its request went out must not leave the account needing Apple proof.
    @Test func cancellationBeforeSendingLeavesNoMarker() async throws {
        let harness = try harness()
        let (accountKey, familyKey) = try seed(harness)
        var exchanges = 0
        let refresher = SessionRefresher(
            store: harness.store, keychain: harness.keychain, leases: harness.leases
        ) { _ in
            exchanges += 1
            return SessionFixture.credentials(access: "new-access")
        }

        let task = Task { try await refresher.refresh(accountKey: accountKey, familyKey: familyKey) }
        task.cancel()
        await #expect(throws: CancellationError.self) { try await task.value }

        #expect(exchanges == 0)
        #expect(try harness.store.refreshAttemptState(for: accountKey.leaseKey) == .idle)
    }

    /// The lease is what stops a second process rotating the same token. A
    /// request allowed to outlive it could come back after another process
    /// took over — and its successful answer would be thrown away.
    @Test func identityRequestsGiveUpWellBeforeTheLeaseLapses() {
        let configuration = HTTPIdentityClient().sessionConfiguration
        #expect(configuration.timeoutIntervalForRequest <= HTTPIdentityClient.requestTimeout)
        #expect(configuration.timeoutIntervalForResource <= HTTPIdentityClient.requestTimeout)
        #expect(HTTPIdentityClient.requestTimeout <= SessionRefresher.leaseDuration * 2 / 3)
    }

    /// Exactly one refresh happens when two holders race. The loser does not
    /// send a second request with the same parent token.
    @Test func onlyOneHolderRefreshesWhenTwoRace() async throws {
        let harness = try harness()
        let (accountKey, familyKey) = try seed(harness)
        var exchanges = 0

        // A competing process is already holding the account lease.
        _ = try harness.leases.acquire(
            scope: .account(accountKey.leaseKey), ownerID: UUID(), now: .now, ttl: 60
        )

        let refresher = SessionRefresher(
            store: harness.store, keychain: harness.keychain, leases: harness.leases
        ) { _ in
            exchanges += 1
            return SessionFixture.credentials(access: "should-not-happen")
        }

        await #expect(throws: SessionRefresher.RefreshError.lockUnavailable) {
            try await refresher.refresh(accountKey: accountKey, familyKey: familyKey)
        }
        #expect(exchanges == 0)
        #expect(try harness.keychain.loadFamily(familyKey)?.accessToken == "access-token")
    }

    /// A session that is gone cannot be refreshed into existence.
    @Test func missingSessionRequiresFreshProof() async throws {
        let harness = try harness()
        let accountKey = SessionFixture.accountKey()
        try harness.store.markAccountAuthenticated(accountKey.leaseKey, credentialVersion: UUID())
        let refresher = SessionRefresher(
            store: harness.store, keychain: harness.keychain, leases: harness.leases
        ) { _ in SessionFixture.credentials() }

        await #expect(throws: SessionRefresher.RefreshError.uncertainAttemptRequiresFreshProof) {
            try await refresher.refresh(accountKey: accountKey, familyKey: SessionFixture.familyKey())
        }
    }
}

/// Separate protocol class keeps this test's delayed recovery independent of
/// other suites exercising ordinary authentication failures in parallel.
final class LostReplyMutationProtocol: URLProtocol, @unchecked Sendable {
    @MainActor static var handle: ((URLRequest, Data) throws -> (Int, Data))?
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        var body = request.httpBody ?? Data()
        if let stream = request.httpBodyStream {
            stream.open(); defer { stream.close() }
            var buffer = [UInt8](repeating: 0, count: 4096)
            while stream.hasBytesAvailable {
                let count = stream.read(&buffer, maxLength: buffer.count)
                if count <= 0 { break }
                body.append(contentsOf: buffer[0..<count])
            }
        }
        let bytes = body
        Task { @MainActor in
            do {
                let (status, data) = try Self.handle!(request, bytes)
                let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: nil, headerFields: nil)!
                client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
                client?.urlProtocol(self, didLoad: data)
                client?.urlProtocolDidFinishLoading(self)
            } catch { client?.urlProtocol(self, didFailWithError: error) }
        }
    }
    override func stopLoading() {}
}
