import BabyCore
import BabyProtocol
import BabyStore
import Foundation

/// One bounded catch-up with the family server.
///
/// True only when the attempt got all the way through — the outbox sent and
/// every page applied. Not `BackgroundSync.runOnce`'s answer, which says an
/// attempt was *made*: an offline attempt is made too.
@MainActor
public protocol LiveActivityCatchUp: AnyObject, Sendable {
    func catchUp() async -> Bool
}

/// The catch-up over the ordinary sync path: one `SyncCoordinator` attempt,
/// and — when the server asked for a renewed session and got one — the same
/// attempt once more, as the sync engine does.
@MainActor
public final class SyncCoordinatorCatchUp: LiveActivityCatchUp {
    private var coordinator: SyncCoordinator!
    private var renewed = false

    public init(
        store: BabyStore,
        transport: any FamilySyncTransport,
        accountKey: AccountSessionKey,
        renew: @escaping () async throws -> Void,
        onRevoked: @escaping (RealmID) async -> Void = { _ in }
    ) {
        coordinator = SyncCoordinator(
            store: store, transport: transport, accountKey: accountKey, onRevoked: onRevoked,
            onRefreshRequired: { [weak self] in
                self?.renewed = true
                try await renew()
            }
        )
    }

    public func catchUp() async -> Bool {
        renewed = false
        await coordinator.syncOnce(reason: .extensionRequest)
        if renewed, !coordinator.lastAttemptSucceeded, !Task.isCancelled {
            renewed = false
            await coordinator.syncOnce(reason: .extensionRequest)
        }
        return coordinator.lastAttemptSucceeded && !Task.isCancelled
    }
}

/// Decides whether a Lock Screen or Dynamic Island button may act on the
/// family's current state.
///
/// A pushed card is presentation, not a write into the store: what it shows
/// may be older than the family (another caregiver stopped the timer and
/// started a new one), newer than this phone (the push arrived, the sync did
/// not), or from another realm altogether (a family left, an epoch bumped).
/// So before a button mutates anything:
///
/// 1. The card must name the store's realm, and this phone must still be
///    allowed to write there — from the family's current authority, never
///    from the card's own `canWrite`.
/// 2. A pushed card the store has already caught up with — same epoch, the
///    cursor at or past the card's — is decided from the store alone, with
///    no network: a tap on a locked phone acts at once. Only a card ahead of
///    the store needs one bounded catch-up that gets through, after which
///    the store has applied at least the cursor the card was built at.
/// 3. Nothing about the realm may have moved during that await.
/// 4. The lifecycle the card shows — its timer, or its break's session —
///    must still be the one the store has. If it ended or was replaced, the
///    card is reconciled and nothing is written.
///
/// `ready` hands back the realm generation it checked: the action's writes
/// go through the store's write boundary with it, which refuses them if the
/// realm moved in between and rechecks write authority under the same lock.
/// A change racing after that still meets the server's own version checks.
///
/// A token card no push has reached shows only what this phone wrote. It is
/// checked for realm and authority, and otherwise keeps today's path.
@MainActor
public final class LiveActivityActionPreflight: LiveActivityActionGate {
    public static let defaultTimeout: Duration = .seconds(5)

    private let store: BabyStore
    private let catchUp: any LiveActivityCatchUp
    private let sessionIsCurrent: @MainActor () -> Bool
    private let sleep: @Sendable (Duration) async throws -> Void
    private let onFinish: @MainActor () -> Void
    private var finished = false

    /// - Parameters:
    ///   - sessionIsCurrent: whether the family session the catch-up used is
    ///     still this phone's, read after the await.
    ///   - sleep: the timeout's clock. Tests inject their own.
    ///   - onFinish: stops whatever the caller started for the catch-up.
    public init(
        store: BabyStore,
        catchUp: any LiveActivityCatchUp,
        sessionIsCurrent: @escaping @MainActor () -> Bool = { true },
        sleep: @escaping @Sendable (Duration) async throws -> Void = { try await Task.sleep(for: $0) },
        onFinish: @escaping @MainActor () -> Void = {}
    ) {
        self.store = store
        self.catchUp = catchUp
        self.sessionIsCurrent = sessionIsCurrent
        self.sleep = sleep
        self.onFinish = onFinish
    }

    public func prepare(target: LiveActivityActionTarget, timeout: Duration = LiveActivityActionPreflight.defaultTimeout) async -> LiveActivityActionDecision {
        let scope = target.scope
        guard !finished, let kind = TimerKind(rawValue: scope.kindRaw),
              let before = try? store.captureWriteContext() else { return .openApp(reason: .unverifiable) }
        guard Self.realm(before.realm, holds: scope) else { return .wrongRealm }
        guard mayWrite(in: before.realm) else { return .readOnly }

        guard target.isPushed else {
            // This phone's own content: nothing newer to wait for. A cursor
            // from another epoch still makes it another realm's card.
            if let cursor = try? store.syncCursor(), cursor.recoveryEpoch != scope.recoveryEpoch { return .wrongRealm }
            return .ready(before)
        }

        // What this phone has applied already vouches for the card: no
        // network before the write. The server may know more — another
        // caregiver's stop this phone has not pulled — but every timer
        // command names its exact timer and a stale one comes back
        // `superseded` with the server's own record (protocol v1, timer
        // commands), so acting here can never end or replace what runs now.
        if let local = localDecision(target: target, kind: kind, in: before) {
            AppLog.widgets.notice("card action: decided locally, no catch-up")
            return local
        }

        let started = ContinuousClock.now
        let caughtUp = await bounded(timeout) { [catchUp] in await catchUp.catchUp() }
        AppLog.widgets.notice("card action: catch-up \(caughtUp ? "succeeded" : "failed", privacy: .public) in \(Self.milliseconds(since: started)) ms")

        guard let after = try? store.captureWriteContext() else { return .openApp(reason: .unverifiable) }
        guard Self.realm(after.realm, holds: scope) else { return .wrongRealm }
        guard after == before, sessionIsCurrent() else { return .openApp(reason: .sessionChanged) }
        guard caughtUp else { return .openApp(reason: .refreshFailed) }
        guard let cursor = try? store.syncCursor(),
              cursor.serverInstanceID == scope.serverInstanceID, cursor.familyID == scope.familyID else {
            return .openApp(reason: .cardAhead)
        }
        if cursor.recoveryEpoch != scope.recoveryEpoch {
            // A card from an epoch the family has left behind, or one this
            // phone has not reached.
            return cursor.recoveryEpoch > scope.recoveryEpoch ? .wrongRealm : .openApp(reason: .cardAhead)
        }
        guard cursor.sequence >= target.minimumFamilySequence else { return .openApp(reason: .cardAhead) }
        guard mayWrite(in: after.realm) else { return .readOnly }
        guard let lifecycle = try? LiveActivityController.lifecycle(kind: kind, babyID: scope.babyID, in: store) else {
            return .openApp(reason: .unverifiable)
        }
        guard target.names(lifecycle) else { return .alreadyFinished }
        return .ready(after)
    }

    /// One bounded sync after an authorized mutation. Its result is not
    /// waited on beyond the timeout: offline, the mutation is already in the
    /// durable outbox and the app sends it later.
    public func publish(timeout: Duration = LiveActivityActionPreflight.defaultTimeout) async {
        guard !finished else { return }
        let started = ContinuousClock.now
        let published = await bounded(timeout) { [catchUp] in await catchUp.catchUp() }
        AppLog.widgets.notice("card action: publish \(published ? "succeeded" : "failed", privacy: .public) in \(Self.milliseconds(since: started)) ms")
    }

    public func finish() {
        guard !finished else { return }
        finished = true
        onFinish()
    }

    // MARK: Internals

    /// The decision for a pushed card from what the store has applied alone,
    /// or nil when the card shows family state this phone has not reached —
    /// a timer another caregiver started, an epoch not yet seen — which only
    /// a catch-up can settle.
    private func localDecision(target: LiveActivityActionTarget, kind: TimerKind, in context: RealmWriteContext) -> LiveActivityActionDecision? {
        let scope = target.scope
        guard let cursor = try? store.syncCursor(),
              cursor.serverInstanceID == scope.serverInstanceID, cursor.familyID == scope.familyID else { return nil }
        if cursor.recoveryEpoch > scope.recoveryEpoch { return .wrongRealm }
        guard cursor.recoveryEpoch == scope.recoveryEpoch, cursor.sequence >= target.minimumFamilySequence,
              let lifecycle = try? LiveActivityController.lifecycle(kind: kind, babyID: scope.babyID, in: store) else { return nil }
        return target.names(lifecycle) ? .ready(context) : .alreadyFinished
    }

    private static func milliseconds(since start: ContinuousClock.Instant) -> Int64 {
        let elapsed = ContinuousClock.now - start
        return elapsed.components.seconds * 1_000 + elapsed.components.attoseconds / 1_000_000_000_000_000
    }

    private func mayWrite(in realm: RealmID) -> Bool {
        (try? store.writePermission(in: realm))?.canWrite == true
    }

    private static func realm(_ realm: RealmID, holds scope: LiveActivityScope) -> Bool {
        guard case .shared(_, let instance, let family) = realm else { return false }
        return instance == scope.serverInstanceID && family == scope.familyID
    }

    /// Runs `work` against the timeout. When the timeout fires first the work
    /// is cancelled — the transport's request with it — and either way both
    /// are awaited before this returns, so nothing started here keeps running
    /// behind the intent. Work cut short counts as no catch-up.
    private func bounded(_ timeout: Duration, _ work: @escaping @MainActor @Sendable () async -> Bool) async -> Bool {
        let sleep = self.sleep
        let worker = Task { @MainActor in await work() }
        let deadline = Task { @MainActor in
            do { try await sleep(timeout) } catch { return }
            worker.cancel()
        }
        let succeeded = await withTaskCancellationHandler {
            await worker.value
        } onCancel: {
            worker.cancel()
        }
        let cutShort = worker.isCancelled
        deadline.cancel()
        await deadline.value
        return succeeded && !cutShort
    }
}
