import BabyProtocol
import BabyStore
import Foundation

/// A cursor-only hint from `/v1/live`.
///
/// It carries no records on purpose. A hint is a nudge to go and sync over
/// HTTPS, never a source of state — applying anything from a socket frame
/// would bypass the receipt and cursor rules that make sync safe.
public struct LiveHint: Codable, Sendable, Equatable {
    public let familyID: UUID
    public let cursor: Cursor
    enum CodingKeys: String, CodingKey { case familyID = "family_id", cursor }
    public init(familyID: UUID, cursor: Cursor) {
        self.familyID = familyID
        self.cursor = cursor
    }
}

/// The socket lifecycle, owned by the foreground app alone.
///
/// A widget or an App Intent must never hold one: they are short-lived, they
/// would each open their own connection, and a socket that outlives its
/// process is a wakeup the system charges the user for with nothing to show.
///
/// One runner task dials, listens and redials until `stop()`. It only ever
/// reports: a hint, a fresh connection, or a refused token each become a
/// callback, and the engine decides what to sync.
public actor FamilySyncWebSocket {
    public enum State: Sendable, Equatable { case stopped, connecting, connected, backingOff }

    public private(set) var state: State = .stopped
    public private(set) var reconnectAttempt = 0
    /// The highest hint sequence seen since `start`. One number, not a log:
    /// a socket left open all day would otherwise keep every hint it heard.
    public private(set) var highestHintSequence: Int64?

    private let connector: any LiveSocketConnecting
    private let tokens: any SessionTokenProviding
    private let policy: SyncRetryPolicy
    private let openTimeout: TimeInterval
    private let pingInterval: TimeInterval
    private let pingTimeout: TimeInterval
    private let stableAfter: TimeInterval
    private let sleep: @Sendable (TimeInterval) async throws -> Void
    private let uptime: @Sendable () -> TimeInterval
    private let jitter: @Sendable (ClosedRange<TimeInterval>) -> TimeInterval
    private let onHint: @Sendable (Cursor) async -> Void
    private let onConnected: @Sendable () async -> Void
    private let onUnauthorized: @Sendable () async -> Bool
    private let onStateChange: @Sendable (State) async -> Void

    private var realm: RealmID?
    private var runner: Task<Void, Never>?
    /// Bumped by every start and stop. A runner acts on the shared state only
    /// while this is still its own number: one that was stopped and replaced
    /// during a callback must not halt, or clear the socket of, its successor.
    private var generation = 0
    private var socket: (any LiveSocket)?
    private var connectedAt: TimeInterval?
    /// Set by a 400/404/426. The route will not appear while this engine
    /// lives, so the family stays on polling rather than redialling a refusal.
    private var unsupported = false

    /// Marks the runner's own task, so a `stop()` reached from inside one of
    /// its callbacks does not wait for the runner — which is waiting for it.
    @TaskLocal private static var runnerOwner: ObjectIdentifier?

    /// - Parameters:
    ///   - onHint: a hint for this family arrived; the engine decides whether
    ///     its cursor is already covered. Return quickly — coalesce, do not
    ///     await a whole sync — because the runner reads no further frames,
    ///     and `stop()` cannot finish, until it returns.
    ///   - onConnected: a socket opened. Whatever happened while it was down
    ///     produced no hint, so the reconnect is itself a reason to sync.
    ///     Return quickly, for the same reason as `onHint`.
    ///   - onUnauthorized: the server refused the token or the authorization
    ///     (401, 403, close 1008). One HTTP sync renews or surfaces why.
    ///     Return `false` when dialling again cannot help — the parent must
    ///     sign in — and the socket stops.
    public init(
        connector: any LiveSocketConnecting,
        tokens: any SessionTokenProviding,
        policy: SyncRetryPolicy = SyncRetryPolicy(base: 1, ceiling: 60, maximumAttempts: .max),
        openTimeout: TimeInterval = 10,
        pingInterval: TimeInterval = 30,
        pingTimeout: TimeInterval = 10,
        stableAfter: TimeInterval = 30,
        sleep: @escaping @Sendable (TimeInterval) async throws -> Void = { try await Task.sleep(for: .seconds($0)) },
        uptime: @escaping @Sendable () -> TimeInterval = { ProcessInfo.processInfo.systemUptime },
        jitter: @escaping @Sendable (ClosedRange<TimeInterval>) -> TimeInterval = { .random(in: $0) },
        onHint: @escaping @Sendable (Cursor) async -> Void,
        onConnected: @escaping @Sendable () async -> Void,
        onUnauthorized: @escaping @Sendable () async -> Bool,
        onStateChange: @escaping @Sendable (State) async -> Void = { _ in }
    ) {
        self.connector = connector
        self.tokens = tokens
        self.policy = policy
        self.openTimeout = openTimeout
        self.pingInterval = pingInterval
        self.pingTimeout = pingTimeout
        self.stableAfter = stableAfter
        self.sleep = sleep
        self.uptime = uptime
        self.jitter = jitter
        self.onHint = onHint
        self.onConnected = onConnected
        self.onUnauthorized = onUnauthorized
        self.onStateChange = onStateChange
    }

    /// Opens the socket only when the server said it has one, and only a
    /// server of this realm's instance.
    ///
    /// Without the advertised capability the client stays polling-only. Dialling
    /// `/v1/live` regardless would be guessing at an endpoint that may not
    /// exist on a self-hosted build. Idempotent: the server allows one socket
    /// per device, so a second runner would only collect 429s. A start that
    /// fails the gate stops a socket already running.
    ///
    /// The owner must always call `stop()`: the runner task holds this actor
    /// until it exits, and nothing else ends it.
    public func start(realm: RealmID, serverInfo: ServerInfo) async -> Bool {
        guard serverInfo.advertisesLiveHints, !unsupported,
              case .shared(_, let instanceID, _) = realm, instanceID == serverInfo.serverInstanceID
        else {
            await stop()
            return false
        }
        if runner != nil, self.realm == realm { return true }
        if runner != nil { await stop() }
        self.realm = realm
        generation += 1
        let mine = generation
        await transition(to: .connecting)
        runner = Task { await self.run(mine) }
        return true
    }

    /// Closes with 1000, which frees the server's single device slot at once
    /// rather than after its ping timeout, and returns once the runner has
    /// finished, so nothing the socket hears afterwards can start a sync.
    public func stop() async {
        let finishing = runner
        runner = nil
        generation += 1
        realm = nil
        reconnectAttempt = 0
        highestHintSequence = nil
        connectedAt = nil
        socket?.close(.normalClosure)
        socket = nil
        finishing?.cancel()
        await transition(to: .stopped)
        if let finishing, Self.runnerOwner != ObjectIdentifier(self) {
            await finishing.value
        }
    }

    func markConnected() async {
        guard state != .stopped else { return }
        // The attempt count is NOT reset here. A socket that opens and is
        // closed at once would otherwise redial about once a second, straight
        // into the server's twelve-a-minute device limit.
        connectedAt = uptime()
        await transition(to: .connected)
        // Checked again after the suspension: a stop during the state
        // callback means there is no connection left to sync for.
        guard state == .connected else { return }
        await onConnected()
    }

    /// Backs off after a drop, bounded, and never gives up while the app is
    /// foregrounded — a socket that stopped retrying looks identical to a
    /// family that stopped logging.
    func handleDisconnect() async -> TimeInterval {
        guard state != .stopped else { return 0 }
        await transition(to: .backingOff)
        reconnectAttempt += 1
        return policy.delay(forAttempt: reconnectAttempt, random: jitter)
    }

    /// Applies nothing. A hint for another family is ignored outright; a hint
    /// for this one is passed on for the engine to sync.
    ///
    /// Hints may arrive out of order or be duplicated, and that is fine
    /// precisely because they are not state: each is passed on, and sync
    /// itself is what converges.
    @discardableResult
    func handle(_ hint: LiveHint) async -> Bool {
        guard state != .stopped, case .shared(_, _, let familyID)? = realm, hint.familyID == familyID else { return false }
        highestHintSequence = max(highestHintSequence ?? hint.cursor.sequence, hint.cursor.sequence)
        await onHint(hint.cursor)
        return true
    }

    /// The exact subprotocol and capabilities the server expects.
    public static let subprotocol = "baby-tracker.v1"
    public static let capabilities = ["records-v1", "live-hints-v1"]

    // MARK: The runner

    private func isLive(_ mine: Int) -> Bool {
        !Task.isCancelled && generation == mine && state != .stopped
    }

    private func run(_ mine: Int) async {
        await Self.$runnerOwner.withValue(ObjectIdentifier(self)) {
            await self.dialUntilStopped(mine)
        }
    }

    private func dialUntilStopped(_ mine: Int) async {
        while isLive(mine) {
            guard let token = await tokens.currentAccessToken() else {
                // No session, nothing to dial with. The next foreground
                // start tries again.
                await halt(mine)
                return
            }
            guard isLive(mine) else { return }
            var delay: TimeInterval = 0
            do {
                let opened = try await open(token: token)
                guard isLive(mine) else {
                    opened.close(.normalClosure)
                    return
                }
                socket = opened
                await markConnected()
                try await listen(to: opened)
            } catch {
                guard isLive(mine) else { return }
                socket = nil
                guard let next = await decide(error, mine) else { return }
                delay = next
            }
            guard isLive(mine) else { return }
            if delay > 0 {
                do { try await sleep(delay) } catch { return }
            }
            guard isLive(mine) else { return }
            await transition(to: .connecting)
        }
    }

    /// Dials, but gives up after `openTimeout`. A black-holed connect would
    /// otherwise sit out URLSession's 60 s request timeout before backing off.
    private func open(token: String) async throws -> any LiveSocket {
        let (connector, sleep, timeout) = (connector, self.sleep, openTimeout)
        return try await withThrowingTaskGroup(of: (any LiveSocket)?.self) { group in
            group.addTask { try await connector.open(token: token) }
            group.addTask { try await sleep(timeout); return nil }
            let first: (any LiveSocket)??
            do {
                first = try await group.next()
            } catch {
                group.cancelAll()
                throw error
            }
            group.cancelAll()
            guard case let socket?? = first else {
                // Timed out. The cancelled dial may still have opened in the
                // meantime; close anything it hands back.
                while let late = try? await group.next() { late?.close(.goingAway) }
                throw LiveSocketError.network
            }
            return socket
        }
    }

    /// What to do after a failed dial or a lost socket: how long to wait, or
    /// `nil` to stop dialling.
    private func decide(_ error: any Error, _ mine: Int) async -> TimeInterval? {
        // Only a socket that stayed up earns a fresh backoff.
        if let connectedAt, uptime() - connectedAt >= stableAfter { reconnectAttempt = 0 }
        connectedAt = nil
        switch error as? LiveSocketError ?? .network {
        case .closed(code: 1000, _):
            // The server's lifetime cap (15 min, or the token's expiry), not
            // a failure: redial at once and do not count it.
            reconnectAttempt = 0
            return 0
        case .unauthorized, .forbidden, .closed(code: 1008, _):
            // A stale token, or authorization or the epoch changed. One HTTP
            // sync renews the token or surfaces the revocation; then back
            // off rather than redial into the same refusal.
            let keepDialling = await onUnauthorized()
            guard isLive(mine) else { return nil }
            guard keepDialling else {
                await halt(mine)
                return nil
            }
            return await handleDisconnect()
        case .rateLimited(let retryAfter):
            return max(retryAfter, await handleDisconnect())
        case .unsupported:
            unsupported = true
            await halt(mine)
            return nil
        case .closed, .unavailable, .network:
            return await handleDisconnect()
        }
    }

    /// Reads frames until the socket fails, pinging alongside: a pong that
    /// never comes is half-open TCP after a network change, which `receive()`
    /// alone could take minutes to notice.
    private func listen(to socket: any LiveSocket) async throws {
        let (interval, timeout, sleep) = (pingInterval, pingTimeout, self.sleep)
        try await withThrowingTaskGroup(of: Void.self) { group in
            group.addTask {
                while true {
                    try await sleep(interval)
                    try await Self.ping(socket, timeout: timeout, sleep: sleep)
                }
            }
            group.addTask {
                while true {
                    let frame = try await socket.receive()
                    await self.received(frame)
                }
            }
            // Whichever ends first ends both: closing releases a pending
            // receive or ping that cancellation alone would not.
            defer {
                socket.close(.goingAway)
                group.cancelAll()
            }
            try await group.next()
        }
    }

    private static func ping(
        _ socket: any LiveSocket, timeout: TimeInterval,
        sleep: @escaping @Sendable (TimeInterval) async throws -> Void
    ) async throws {
        try await withThrowingTaskGroup(of: Bool.self) { group in
            group.addTask { try await socket.ping(); return true }
            group.addTask { try await sleep(timeout); return false }
            defer { group.cancelAll() }
            guard try await group.next() == true else {
                socket.close(.goingAway)
                throw LiveSocketError.network
            }
        }
    }

    /// A frame that is not a hint is ignored rather than dropping the socket:
    /// hints carry no state, so a missed one costs at most the safety poll.
    private func received(_ frame: Data) async {
        guard let hint = try? JSONDecoder().decode(LiveHint.self, from: frame) else { return }
        await handle(hint)
    }

    /// Stops from inside the runner, leaving `runner` clear for the next
    /// foreground `start`.
    private func halt(_ mine: Int) async {
        guard generation == mine else { return }
        runner = nil
        realm = nil
        socket = nil
        connectedAt = nil
        await transition(to: .stopped)
    }

    private func transition(to new: State) async {
        guard state != new else { return }
        state = new
        await onStateChange(new)
    }
}
