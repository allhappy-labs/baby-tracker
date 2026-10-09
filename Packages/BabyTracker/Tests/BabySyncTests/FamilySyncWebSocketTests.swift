import BabyCore
import BabyProtocol
import BabyStore
import Foundation
import Testing
@testable import BabySync

@Suite("Live hint socket lifecycle")
struct FamilySyncWebSocketTests {
    private func serverInfo(liveHints: Bool, instanceID: UUID = SyncFixtures.instanceID) -> ServerInfo {
        ServerInfo(
            serverInstanceID: instanceID,
            canonicalOrigin: "https://sync.example",
            recoveryEpoch: 1,
            capabilities: liveHints ? ["records-v1", "live-hints-v1"] : ["records-v1"],
            appleAuthorizationMode: "hosted",
            maxItemsPerPage: 200, maxEncodedBytes: 1_000_000,
            sourceVersion: "1.0.0", sourceURL: ""
        )
    }

    /// The socket is opened only when the server says it has one. Dialling
    /// /v1/live regardless would be guessing at an endpoint a self-hosted
    /// build may not serve.
    @Test func socketOpensOnlyWhenTheCapabilityIsAdvertised() async {
        let live = LiveHarness()

        #expect(await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: false)) == false)
        #expect(await live.socket.state == .stopped)

        #expect(await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true)))
        #expect(await live.socket.state == .connecting)
        await live.socket.stop()
    }

    /// A server answering with another instance is not the one this realm
    /// belongs to, so its hints would be about somebody else's data.
    @Test func socketRefusesAServerOfAnotherInstance() async {
        let live = LiveHarness()

        let other = serverInfo(liveHints: true, instanceID: UUID())
        #expect(await live.socket.start(realm: SyncFixtures.realm, serverInfo: other) == false)
        #expect(await live.socket.state == .stopped)
        #expect(live.connector.dialledTokens.isEmpty)
    }

    @Test func capabilitiesAndSubprotocolAreExact() {
        #expect(FamilySyncWebSocket.capabilities == ["records-v1", "live-hints-v1"])
        #expect(FamilySyncWebSocket.subprotocol == "baby-tracker.v1")
    }

    /// Reconnecting is itself a reason to sync: whatever happened while the
    /// socket was down produced no hint to catch up on.
    @Test func connectingTriggersASync() async {
        let live = LiveHarness()
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))

        await live.socket.markConnected()

        #expect(live.events.connected == 1)
        #expect(await live.socket.state == .connected)
        await live.socket.stop()
    }

    /// A hint for this family syncs; a hint for another family is ignored
    /// outright rather than syncing the wrong realm.
    @Test func onlyThisFamilysHintsTriggerASync() async {
        let live = LiveHarness()
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))

        let mine = LiveHint(familyID: SyncFixtures.familyID, cursor: SyncFixtures.cursor(4))
        let theirs = LiveHint(familyID: UUID(), cursor: SyncFixtures.cursor(9))

        #expect(await live.socket.handle(mine))
        #expect(await live.socket.handle(theirs) == false)
        #expect(live.events.hints == [SyncFixtures.cursor(4)])
        await live.socket.stop()
    }

    /// Hints may arrive out of order or twice. That is fine precisely because
    /// they carry no state: each one is passed on, and sync is what converges.
    /// Only the highest sequence is remembered, so a socket left open all day
    /// does not grow a log of every hint it ever saw.
    @Test func reorderedAndDuplicateHintsArePassedOnAndOnlyTheHighestIsKept() async {
        let live = LiveHarness()
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))

        #expect(await live.socket.handle(LiveHint(familyID: SyncFixtures.familyID, cursor: SyncFixtures.cursor(9))))
        #expect(await live.socket.handle(LiveHint(familyID: SyncFixtures.familyID, cursor: SyncFixtures.cursor(4))))
        #expect(await live.socket.handle(LiveHint(familyID: SyncFixtures.familyID, cursor: SyncFixtures.cursor(4))))

        #expect(live.events.hints.map(\.sequence) == [9, 4, 4])
        #expect(await live.socket.highestHintSequence == 9)
        await live.socket.stop()
    }

    /// Backoff is bounded and never gives up while foregrounded — a socket
    /// that stopped retrying looks exactly like a family that stopped logging.
    @Test func disconnectBacksOffAndKeepsTrying() async {
        let live = LiveHarness(policy: SyncRetryPolicy(base: 1, ceiling: 8))
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))

        for expected in 1...5 {
            _ = await live.socket.handleDisconnect()
            #expect(await live.socket.reconnectAttempt == expected)
        }
        #expect(await live.socket.state == .backingOff)
        await live.socket.stop()
    }

    /// A stopped socket stays stopped: backgrounding must not leave a
    /// reconnect loop running.
    @Test func stoppingEndsReconnection() async {
        let live = LiveHarness()
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))
        _ = await live.socket.handleDisconnect()

        await live.socket.stop()

        #expect(await live.socket.handleDisconnect() == 0)
        #expect(await live.socket.state == .stopped)
        #expect(await live.socket.reconnectAttempt == 0)
    }

    /// A hint arriving after stop does nothing.
    @Test func hintsAfterStopAreIgnored() async {
        let live = LiveHarness()
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))
        await live.socket.stop()

        #expect(await live.socket.handle(LiveHint(familyID: SyncFixtures.familyID, cursor: SyncFixtures.cursor(1))) == false)
        #expect(live.events.hints.isEmpty)
    }

    /// The short poll is only for servers without hints; with a live socket
    /// only the slow safety poll remains.
    @Test func pollingIsOnlyForServersWithoutHints() {
        #expect(!SyncScheduler.needsPolling(serverInfo: serverInfo(liveHints: true)))
        #expect(SyncScheduler.needsPolling(serverInfo: serverInfo(liveHints: false)))
    }

    // MARK: The wire

    /// The server's own hint fixture decodes, so the two cannot drift apart
    /// without a test noticing. No Swift test read this file before.
    @Test func liveHintDecodesTheServersFixture() throws {
        let data = try Data(contentsOf: ServerErrorEnvelopeTests.fixtures.appending(path: "v1/live-hint.json"))
        let hint = try JSONDecoder().decode(LiveHint.self, from: data)

        let family = UUID(uuidString: "b1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!
        #expect(hint == LiveHint(familyID: family, cursor: Cursor(
            serverInstanceID: UUID(uuidString: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d")!,
            recoveryEpoch: 1, familyID: family, sequence: 42
        )))
    }

    /// The upgrade request carries exactly what `/v1/live` checks: a bearer
    /// header (never a query or a cookie, which the server refuses), the
    /// protocol and capability headers, one subprotocol, and no Origin — a
    /// native client may leave it out, and a wrong one is a 400.
    @Test(arguments: [
        ("https://sync.example", "wss://sync.example/v1/live"),
        ("https://sync.example:8443", "wss://sync.example:8443/v1/live"),
    ])
    func upgradeRequestIsExactlyWhatTheServerChecks(origin: String, expected: String) throws {
        let request = URLSessionLiveSocketConnector.request(origin: try ServerOrigin(validating: origin), token: "tok-1")

        #expect(request.url?.absoluteString == expected)
        #expect(request.url?.query == nil)
        #expect(request.value(forHTTPHeaderField: "Authorization") == "Bearer tok-1")
        #expect(request.value(forHTTPHeaderField: "X-Baby-Tracker-Protocol") == "v1")
        #expect(request.value(forHTTPHeaderField: "X-Baby-Tracker-Capabilities") == "records-v1,live-hints-v1")
        #expect(request.value(forHTTPHeaderField: "Sec-WebSocket-Protocol") == "baby-tracker.v1")
        #expect(request.value(forHTTPHeaderField: "Origin") == nil)
        #expect(request.value(forHTTPHeaderField: "Cookie") == nil)
        #expect(request.httpShouldHandleCookies == false)
        #expect(request.allHTTPHeaderFields?.count == 4)
    }

    /// Each refused upgrade becomes the one error the runner decides on.
    @Test func refusedUpgradeStatusesMapToLiveSocketErrors() {
        typealias C = URLSessionLiveSocketConnector
        #expect(C.error(status: 401, retryAfter: nil) == .unauthorized)
        #expect(C.error(status: 403, retryAfter: nil) == .forbidden)
        #expect(C.error(status: 429, retryAfter: "7") == .rateLimited(7))
        #expect(C.error(status: 429, retryAfter: nil) == .rateLimited(5))
        #expect(C.error(status: 429, retryAfter: "soon") == .rateLimited(5))
        #expect(C.error(status: 400, retryAfter: nil) == .unsupported)
        #expect(C.error(status: 404, retryAfter: nil) == .unsupported)
        #expect(C.error(status: 426, retryAfter: nil) == .unsupported)
        #expect(C.error(status: 503, retryAfter: nil) == .unavailable)
        #expect(C.error(status: 502, retryAfter: nil) == .unavailable)
        #expect(C.upgradeError(response: nil) == .network)
        let refused = HTTPURLResponse(
            url: URL(string: "wss://sync.example/v1/live")!, statusCode: 429,
            httpVersion: "HTTP/1.1", headerFields: ["Retry-After": "12"]
        )
        #expect(C.upgradeError(response: refused) == .rateLimited(12))
    }

    /// URLSession may call a ping's pong handler more than once on a failed
    /// connection. Resuming a continuation twice crashes the app, so only the
    /// first call counts.
    @Test func aPongHandlerCalledTwiceResumesOnce() async throws {
        try await URLSessionLiveSocketConnector.awaitPong { handler in
            handler(nil)
            handler(URLError(.networkConnectionLost))
        }
        await #expect(throws: URLError.self) {
            try await URLSessionLiveSocketConnector.awaitPong { handler in
                handler(URLError(.networkConnectionLost))
                handler(nil)
            }
        }
    }

    // MARK: The runner

    /// Opening syncs once; a hint for this family is passed on with its
    /// cursor; one for another family, or a frame that is not a hint, is not.
    @Test func openedSocketSyncsOnceAndPassesOnThisFamilysHints() async throws {
        let live = LiveHarness()
        let socket = FakeLiveSocket()
        live.connector.script = [.socket(socket)]
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))

        try await until { await live.socket.state == .connected }
        socket.deliver(LiveHint(familyID: UUID(), cursor: SyncFixtures.cursor(8)))
        socket.deliver(Data("not a hint".utf8))
        socket.deliver(LiveHint(familyID: SyncFixtures.familyID, cursor: SyncFixtures.cursor(9)))

        try await until { !live.events.hints.isEmpty }
        #expect(live.events.connected == 1)
        #expect(live.events.hints == [SyncFixtures.cursor(9)])
        #expect(live.connector.dialledTokens == ["token-1"])
        #expect(live.events.states == [.connecting, .connected])
        await live.socket.stop()
    }

    /// A 401 means the token went stale. One HTTP sync renews it through the
    /// engine's existing path, and the socket redials with the new token.
    @Test func unauthorizedUpgradeRenewsThroughASyncThenRedialsWithTheNewToken() async throws {
        let live = LiveHarness()
        live.connector.script = [.fail(.unauthorized), .socket(FakeLiveSocket())]
        live.events.onUnauthorized = { live.tokens.token = "token-2"; return true }
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))

        try await until { await live.socket.state == .connected }
        #expect(live.events.unauthorized == 1)
        #expect(live.connector.dialledTokens == ["token-1", "token-2"])
        await live.socket.stop()
    }

    /// When the renewal cannot help — the parent must sign in again — the
    /// socket stops dialling rather than burning the device's attempts.
    @Test func unauthorizedThatNeedsSignInStopsDialling() async throws {
        let live = LiveHarness()
        live.connector.script = [.fail(.unauthorized), .socket(FakeLiveSocket())]
        live.events.onUnauthorized = { false }
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))

        try await until { live.events.states.last == .stopped }
        #expect(live.connector.dialledTokens == ["token-1"])
        #expect(await live.socket.state == .stopped)
    }

    /// Without a session there is nothing to dial with.
    @Test func noTokenMeansNoDial() async throws {
        let live = LiveHarness()
        live.tokens.token = nil
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))

        try await until { live.events.states.last == .stopped }
        #expect(live.connector.dialledTokens.isEmpty)
    }

    /// Capacity or rate refusals name their own wait; redialling sooner would
    /// only spend another of the device's twelve attempts a minute.
    @Test func rateLimitedWaitsAtLeastRetryAfter() async throws {
        let live = LiveHarness()
        live.connector.script = [.fail(.rateLimited(7))]
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))

        try await until { live.connector.dialledTokens.count == 2 }
        #expect(live.sleeper.backoffs.count == 1)
        #expect(live.sleeper.backoffs.allSatisfy { $0 >= 7 })
        await live.socket.stop()
    }

    /// A server or proxy without the route will not grow one: stop dialling
    /// for this engine's lifetime and leave the family on polling.
    @Test func unsupportedStopsForGood() async throws {
        let live = LiveHarness()
        live.connector.script = [.fail(.unsupported), .socket(FakeLiveSocket())]
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))

        try await until { live.events.states.last == .stopped }
        #expect(await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true)) == false)
        try await Task.sleep(for: .milliseconds(20))
        #expect(live.connector.dialledTokens.count == 1)
        #expect(await live.socket.state == .stopped)
    }

    /// 1000 is the server's lifetime cap, not a failure: redial at once,
    /// without a wait and without counting it.
    @Test func normalCloseRedialsImmediatelyWithoutCountingAFailure() async throws {
        let live = LiveHarness()
        let first = FakeLiveSocket()
        live.connector.script = [.socket(first), .socket(FakeLiveSocket())]
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))
        try await until { await live.socket.state == .connected }

        first.fail(.closed(code: 1000, reason: "maximum lifetime"))

        try await until { live.connector.dialledTokens.count == 2 && live.events.connected == 2 }
        #expect(live.sleeper.backoffs.isEmpty)
        #expect(await live.socket.reconnectAttempt == 0)
        await live.socket.stop()
    }

    /// 1008 means authorization or the epoch changed underneath the socket.
    /// One HTTP sync surfaces which; then back off rather than redial into
    /// the same refusal.
    @Test func policyCloseSyncsOnceThenBacksOff() async throws {
        let live = LiveHarness()
        let first = FakeLiveSocket()
        live.connector.script = [.socket(first)]
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))
        try await until { await live.socket.state == .connected }

        first.fail(.closed(code: 1008, reason: "authorization changed"))

        try await until { live.connector.dialledTokens.count == 2 }
        #expect(live.events.unauthorized == 1)
        #expect(live.sleeper.backoffs == [1])
        #expect(await live.socket.reconnectAttempt == 1)
        await live.socket.stop()
    }

    /// A socket that opens and dies at once must not reset its own backoff:
    /// that is the loop that cycles at about one a second into the server's
    /// device limit. Only thirty seconds up earns a fresh start.
    @Test func backoffOnlyResetsAfterTheSocketStayedUp() async throws {
        let live = LiveHarness()
        let flaky = (0..<3).map { _ in FakeLiveSocket(failing: .closed(code: 1013, reason: "slow consumer")) }
        let steady = FakeLiveSocket()
        live.connector.script = flaky.map { .socket($0) } + [.socket(steady)]
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))

        try await until { live.connector.dialledTokens.count == 4 && live.events.connected == 4 }
        #expect(live.sleeper.backoffs == [1, 2, 4])
        #expect(await live.socket.reconnectAttempt == 3)

        live.clock.advance(by: 31)
        steady.fail(.closed(code: 1001, reason: "going away"))

        try await until { live.connector.dialledTokens.count == 5 }
        #expect(live.sleeper.backoffs == [1, 2, 4, 1])
        #expect(await live.socket.reconnectAttempt == 1)
        await live.socket.stop()
    }

    /// A pong that never comes means half-open TCP after a network change.
    /// Waiting for `receive()` to notice could take minutes.
    @Test func pingTimeoutClosesTheSocketAndRedials() async throws {
        let live = LiveHarness(blockingSleeps: [LiveHarness.openTimeout])
        let silent = FakeLiveSocket(answersPings: false)
        live.connector.script = [.socket(silent)]
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))

        try await until { live.connector.dialledTokens.count == 2 }
        #expect(silent.closeCodes.contains(.goingAway))
        #expect(live.sleeper.all.filter { $0 != LiveHarness.openTimeout }.starts(with: [30, 10]))
        await live.socket.stop()
    }

    /// Stopping closes cleanly — that frees the server's single device slot
    /// at once — and returns only when the runner has finished, so nothing
    /// the socket hears afterwards can start a sync.
    @Test func stopClosesNormallyAndFinishesTheRunner() async throws {
        let live = LiveHarness()
        let socket = FakeLiveSocket()
        live.connector.script = [.socket(socket)]
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))
        try await until { await live.socket.state == .connected }

        await live.socket.stop()

        #expect(socket.closeCodes.first == .normalClosure)
        socket.deliver(LiveHint(familyID: SyncFixtures.familyID, cursor: SyncFixtures.cursor(3)))
        try await Task.sleep(for: .milliseconds(20))
        #expect(live.events.hints.isEmpty)
        #expect(live.connector.dialledTokens.count == 1)
        #expect(await live.socket.state == .stopped)
    }

    /// The engine may stop the socket from inside a callback the runner is
    /// awaiting — a revocation found by the unauthorized sync, say. Waiting
    /// for the runner there would wait for itself.
    @Test func stoppingFromInsideACallbackDoesNotDeadlock() async throws {
        let live = LiveHarness()
        live.connector.script = [.fail(.forbidden)]
        let socket = live.socket
        live.events.onUnauthorized = { await socket.stop(); return true }
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))

        try await until { live.events.unauthorized == 1 && live.events.states.last == .stopped }
        try await Task.sleep(for: .milliseconds(20))
        #expect(live.connector.dialledTokens.count == 1)
    }

    /// Starting twice does not run two sockets: the server allows one per
    /// device, and the second would only collect 429s.
    @Test func startingTwiceKeepsOneSocket() async throws {
        let live = LiveHarness()
        live.connector.script = [.socket(FakeLiveSocket())]
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))
        try await until { await live.socket.state == .connected }

        #expect(await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true)))
        try await Task.sleep(for: .milliseconds(20))
        #expect(live.connector.dialledTokens.count == 1)
        #expect(await live.socket.state == .connected)
        await live.socket.stop()
    }

    /// A dial into a black hole must not wait out URLSession's 60 s request
    /// timeout: give up after the open timeout and back off like any drop.
    @Test func openThatNeverAnswersTimesOutAndBacksOff() async throws {
        let live = LiveHarness(blockingSleeps: [30])
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))

        try await until { live.connector.dialledTokens.count >= 2 }
        #expect(live.sleeper.all.starts(with: [LiveHarness.openTimeout]))
        #expect(live.sleeper.backoffs.first == 1)
        await live.socket.stop()
    }

    /// Stopped while announcing the connection: the sync it would have
    /// triggered is for a socket that no longer exists.
    @Test func stoppedWhileConnectingDoesNotReportTheConnection() async throws {
        let live = LiveHarness()
        live.connector.script = [.socket(FakeLiveSocket())]
        let socket = live.socket
        live.events.onStateChange = { if $0 == .connected { await socket.stop() } }
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))

        try await until { live.events.states.last == .stopped }
        try await Task.sleep(for: .milliseconds(20))
        #expect(live.events.connected == 0)
    }

    /// A runner that was stopped and replaced mid-callback must leave its
    /// successor alone: halting on its way out would stop the new socket.
    @Test func aReplacedRunnerLeavesItsSuccessorRunning() async throws {
        let live = LiveHarness()
        live.connector.script = [.fail(.forbidden), .socket(FakeLiveSocket())]
        let (socket, info) = (live.socket, serverInfo(liveHints: true))
        live.events.onUnauthorized = {
            await socket.stop()
            _ = await socket.start(realm: SyncFixtures.realm, serverInfo: info)
            return false
        }
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: info)

        try await until { await live.socket.state == .connected }
        try await Task.sleep(for: .milliseconds(20))
        #expect(await live.socket.state == .connected)
        #expect(live.connector.dialledTokens.count == 2)
        await live.socket.stop()
    }

    /// A later start that fails the gate — the server dropped the capability
    /// — must not leave the old socket running.
    @Test func aFailedStartStopsTheRunningSocket() async throws {
        let live = LiveHarness()
        let open = FakeLiveSocket()
        live.connector.script = [.socket(open)]
        _ = await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: true))
        try await until { await live.socket.state == .connected }

        #expect(await live.socket.start(realm: SyncFixtures.realm, serverInfo: serverInfo(liveHints: false)) == false)

        #expect(open.closeCodes.first == .normalClosure)
        #expect(await live.socket.state == .stopped)
    }
}

// MARK: - Fakes for the live socket

/// Polls a condition in 1 ms steps, for at most two seconds. The runner is a
/// task of its own, so a test can only watch for it to get somewhere.
func until(_ condition: @Sendable () async -> Bool, sourceLocation: SourceLocation = #_sourceLocation) async throws {
    for _ in 0..<2000 {
        if await condition() { return }
        try await Task.sleep(for: .milliseconds(1))
    }
    Issue.record("condition never became true", sourceLocation: sourceLocation)
}

final class Locked<Value>: @unchecked Sendable {
    private let lock = NSLock()
    private var value: Value
    init(_ value: Value) { self.value = value }
    func callAsFunction<T>(_ body: (inout Value) -> T) -> T {
        lock.lock(); defer { lock.unlock() }
        return body(&value)
    }
}

/// A socket fed by the test. `fail` or `close` ends it; a pending `receive`
/// or `ping` is released with the error, as `URLSessionWebSocketTask` does.
final class FakeLiveSocket: LiveSocket, @unchecked Sendable {
    private struct Inner {
        var buffered: [Data] = []
        var terminal: LiveSocketError?
        var receiver: CheckedContinuation<Data, any Error>?
        var pingers: [CheckedContinuation<Void, any Error>] = []
        var closeCodes: [URLSessionWebSocketTask.CloseCode] = []
    }
    private let inner = Locked(Inner())
    private let answersPings: Bool

    init(answersPings: Bool = true, failing: LiveSocketError? = nil) {
        self.answersPings = answersPings
        inner { $0.terminal = failing }
    }

    var closeCodes: [URLSessionWebSocketTask.CloseCode] { inner { $0.closeCodes } }

    func deliver(_ hint: LiveHint) { deliver(try! JSONEncoder().encode(hint)) }

    func deliver(_ data: Data) {
        let receiver: CheckedContinuation<Data, any Error>? = inner { state in
            guard state.terminal == nil else { return nil }
            if let receiver = state.receiver { state.receiver = nil; return receiver }
            state.buffered.append(data)
            return nil
        }
        receiver?.resume(returning: data)
    }

    func fail(_ error: LiveSocketError) {
        let (receiver, pingers) = inner { state -> (CheckedContinuation<Data, any Error>?, [CheckedContinuation<Void, any Error>]) in
            if state.terminal == nil { state.terminal = error }
            defer { state.receiver = nil; state.pingers = [] }
            return (state.receiver, state.pingers)
        }
        receiver?.resume(throwing: error)
        pingers.forEach { $0.resume(throwing: error) }
    }

    func receive() async throws -> Data {
        try await withCheckedThrowingContinuation { continuation in
            let outcome: Result<Data, any Error>? = inner { state in
                if !state.buffered.isEmpty { return .success(state.buffered.removeFirst()) }
                if let terminal = state.terminal { return .failure(terminal) }
                state.receiver = continuation
                return nil
            }
            if let outcome { continuation.resume(with: outcome) }
        }
    }

    func ping() async throws {
        if answersPings { return }
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, any Error>) in
            let terminal: LiveSocketError? = inner { state in
                if let terminal = state.terminal { return terminal }
                state.pingers.append(continuation)
                return nil
            }
            if let terminal { continuation.resume(throwing: terminal) }
        }
    }

    func close(_ code: URLSessionWebSocketTask.CloseCode) {
        inner { $0.closeCodes.append(code) }
        fail(.closed(code: code.rawValue, reason: ""))
    }
}

/// A scripted dialler. Once the script runs out, `open` waits until the
/// runner is cancelled, like a dial into a server that never answers.
final class FakeLiveSocketConnector: LiveSocketConnecting, @unchecked Sendable {
    enum Outcome { case socket(FakeLiveSocket), fail(LiveSocketError) }
    private let state = Locked((script: [Outcome](), tokens: [String]()))

    var script: [Outcome] {
        get { state { $0.script } }
        set { state { $0.script = newValue } }
    }
    var dialledTokens: [String] { state { $0.tokens } }

    func open(token: String) async throws -> any LiveSocket {
        let next: Outcome? = state { state in
            state.tokens.append(token)
            return state.script.isEmpty ? nil : state.script.removeFirst()
        }
        switch next {
        case .socket(let socket): return socket
        case .fail(let error): throw error
        case nil:
            try await Task.sleep(for: .seconds(3600))
            throw LiveSocketError.network
        }
    }
}

final class FakeLiveTokens: SessionTokenProviding, @unchecked Sendable {
    private let current = Locked<String?>("token-1")
    var token: String? {
        get { current { $0 } }
        set { current { $0 = newValue } }
    }
    func currentAccessToken() async -> String? { token }
    func currentSessionIdentity() async -> SessionIdentity? { nil }
}

/// Records every wait. The ones listed as blocking (the ping interval and
/// the open timeout by default) wait until cancelled, so a connected socket sits still; the
/// rest return at once, so a backoff costs the test nothing.
final class FakeLiveSleeper: @unchecked Sendable {
    let blocking: Set<TimeInterval>
    private let delays = Locked<[TimeInterval]>([])
    init(blocking: Set<TimeInterval>) { self.blocking = blocking }

    var all: [TimeInterval] { delays { $0 } }
    /// The waits that were not the ping interval, ping timeout or open timeout.
    var backoffs: [TimeInterval] { all.filter { ![30, 10, LiveHarness.openTimeout].contains($0) } }

    func sleep(_ delay: TimeInterval) async throws {
        delays { $0.append(delay) }
        if blocking.contains(delay) {
            try await Task.sleep(for: .seconds(3600))
        } else {
            await Task.yield()
            try Task.checkCancellation()
        }
    }
}

final class FakeLiveClock: @unchecked Sendable {
    private let now = Locked<TimeInterval>(1_000)
    func advance(by seconds: TimeInterval) { now { $0 += seconds } }
    func uptime() -> TimeInterval { now { $0 } }
}

final class LiveEvents: @unchecked Sendable {
    private let state = Locked((hints: [Cursor](), connected: 0, unauthorized: 0, states: [FamilySyncWebSocket.State]()))
    private let handler = Locked<@Sendable () async -> Bool>({ true })
    private let stateHook = Locked<@Sendable (FamilySyncWebSocket.State) async -> Void>({ _ in })

    var hints: [Cursor] { state { $0.hints } }
    var connected: Int { state { $0.connected } }
    var unauthorized: Int { state { $0.unauthorized } }
    var states: [FamilySyncWebSocket.State] { state { $0.states } }
    var onUnauthorized: @Sendable () async -> Bool {
        get { handler { $0 } }
        set { handler { $0 = newValue } }
    }

    func hint(_ cursor: Cursor) { state { $0.hints.append(cursor) } }
    func didConnect() { state { $0.connected += 1 } }
    func didRefuse() async -> Bool {
        state { $0.unauthorized += 1 }
        return await onUnauthorized()
    }
    var onStateChange: @Sendable (FamilySyncWebSocket.State) async -> Void {
        get { stateHook { $0 } }
        set { stateHook { $0 = newValue } }
    }
    func changed(_ new: FamilySyncWebSocket.State) async {
        state { $0.states.append(new) }
        await onStateChange(new)
    }
}

/// One socket wired to fakes, with jitter pinned to the top of each window so
/// a backoff sequence is exact: 1, 2, 4, …
struct LiveHarness {
    let connector = FakeLiveSocketConnector()
    let tokens = FakeLiveTokens()
    let sleeper: FakeLiveSleeper
    let clock = FakeLiveClock()
    let events = LiveEvents()
    let socket: FamilySyncWebSocket

    /// Distinct from the ping interval (30) and ping timeout (10), so the
    /// sleeper can tell the three waits apart.
    static let openTimeout: TimeInterval = 20

    init(policy: SyncRetryPolicy = SyncRetryPolicy(base: 1, ceiling: 60, maximumAttempts: .max), blockingSleeps: Set<TimeInterval> = [30, openTimeout]) {
        let sleeper = FakeLiveSleeper(blocking: blockingSleeps)
        let (clock, events) = (clock, events)
        self.sleeper = sleeper
        socket = FamilySyncWebSocket(
            connector: connector, tokens: tokens, policy: policy, openTimeout: Self.openTimeout,
            sleep: { try await sleeper.sleep($0) },
            uptime: { clock.uptime() },
            jitter: { $0.upperBound },
            onHint: { events.hint($0) },
            onConnected: { events.didConnect() },
            onUnauthorized: { await events.didRefuse() },
            onStateChange: { await events.changed($0) }
        )
    }
}

@MainActor
@Suite("Bounded extension sync")
struct BackgroundSyncTests {
    private func make() throws -> (BabyStore, FakeSyncTransport, BackgroundSync) {
        let store = try SyncFixtures.store()
        let transport = FakeSyncTransport()
        let coordinator = SyncCoordinator(store: store, transport: transport, accountKey: SyncFixtures.accountKey)
        return (store, transport, BackgroundSync(coordinator: coordinator, store: store, accountKey: SyncFixtures.accountKey))
    }

    /// Exactly one attempt. An extension is short-lived and budgeted; a retry
    /// loop there spends battery on work the app redoes properly when opened.
    @Test func extensionGetsExactlyOneAttempt() async throws {
        let (_, transport, background) = try make()

        #expect(await background.runOnce(reason: .extensionRequest))

        #expect(background.attempts == 1)
        #expect(transport.requests.count == 1)
    }

    /// The gate is rechecked in this process. An extension may never have seen
    /// the sign-out that happened while it was not running.
    @Test func signedOutAccountSyncsNothingFromAnExtension() async throws {
        let (store, transport, background) = try make()
        try store.markAccountSignedOut(SyncFixtures.accountKey.leaseKey)

        #expect(await background.runOnce(reason: .extensionRequest) == false)

        #expect(background.attempts == 0)
        #expect(transport.requests.isEmpty)
    }

    @Test func localOnlyDeviceSyncsNothingFromAnExtension() async throws {
        let store = try BabyStore.inMemory(dateProvider: TestDateProvider(.now))
        try store.markAccountAuthenticated(SyncFixtures.accountKey.leaseKey, credentialVersion: UUID())
        let transport = FakeSyncTransport()
        let coordinator = SyncCoordinator(store: store, transport: transport, accountKey: SyncFixtures.accountKey)
        let background = BackgroundSync(coordinator: coordinator, store: store, accountKey: SyncFixtures.accountKey)

        #expect(await background.runOnce(reason: .backgroundPush) == false)
        #expect(transport.requests.isEmpty)
    }

    /// One identifier, declared once, so it cannot drift from the Info.plist
    /// entry that permits it.
    @Test func backgroundRefreshIdentifierIsSingular() {
        #expect(BackgroundSync.refreshTaskIdentifier == "com.olhapi.babytracker.sync.refresh")
    }
}
