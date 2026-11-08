import BabyCore
import BabyProtocol
import BabyStore
import CryptoKit
import Foundation
import Observation

/// Turns one family session into records actually moving between phones.
///
/// Everything below it already existed — the outbox, the coordinators, the
/// transports — and none of it ever ran, because nothing decided *when*. This
/// type is that decision: how a device enters the family (the owner brings
/// its history, a joiner starts fresh), and when it talks to the server after
/// that (a moment after a write, as soon as the other phone writes, on a slow
/// poll while the app is open, and never twice at once).
@MainActor
@Observable
public final class FamilySyncEngine {
    public enum Status: Equatable {
        case idle, syncing, upToDate(Date), offline, needsSignIn, failed(String)
        /// Requests have failed for `serverUnreachableAfter` while the phone
        /// has a network. On 2026-10-19 that was a Tailscale tunnel showing
        /// "Connected" and carrying nothing, which "Offline" did not explain.
        case serverUnreachable
    }

    public private(set) var status: Status = .idle {
        didSet {
            switch status {
            case .offline, .serverUnreachable, .syncing: break
            case .idle, .upToDate, .needsSignIn, .failed: offlineSince = nil
            }
        }
    }
    public private(set) var operationalStatus: SyncOperationalStatus = .inactive
    /// From coming to the foreground until the first sync after it has an
    /// answer, or `catchUpHold` has passed. Until then the store may predate
    /// what the other phone did while this one slept: on 2026-11-01 a phone
    /// opened after four silent hours stopped a feed the other parent had
    /// already stopped, then — once the sync landed — the sleep they had
    /// started since.
    public private(set) var isCatchingUp = false

    /// Long enough for a burst of taps to become one request, short enough
    /// that the other caregiver sees the entry "now". With the live socket
    /// this is the largest single delay between a tap here and the row on the
    /// other phone; a burst split in two is harmless, because the outbox
    /// batches and a second push joins the first as a follow-up.
    nonisolated static let defaultDebounce: Duration = .milliseconds(300)
    /// The poll while the app is open and no live socket is up: it is what
    /// makes sync work on a server without hints, or while the socket redials.
    nonisolated static let defaultPollInterval: Duration = .seconds(20)
    /// The poll while the live socket is up. A hint can be lost (the protocol
    /// promises none), so a slow safety poll stays; the socket's own reconnect
    /// every ten minutes or so syncs as well.
    nonisolated static let defaultLivePollInterval: Duration = .seconds(SyncScheduler.pollingInterval)
    /// A ceiling on back-to-back passes in one `syncNow`, so a server that
    /// keeps accepting without ever acknowledging cannot spin the phone.
    static let maximumPassesPerSync = 50
    /// How long requests must keep failing, with a network present, before
    /// the server counts as out of reach rather than this phone as offline.
    /// Longer than a lift ride or a Wi-Fi handover, short enough that a dead
    /// tunnel is named within one feed.
    public nonisolated static let serverUnreachableAfter: TimeInterval = 3 * 60
    /// The longest a tap waits for the catch-up. Past it the server counts as
    /// out of reach at once, and the tap goes ahead on what this phone has.
    public nonisolated static let defaultCatchUpHold: Duration = .seconds(5)

    @ObservationIgnored private let store: BabyStore
    @ObservationIgnored private let familyKey: FamilySessionKey
    @ObservationIgnored private let importClient: any ImportResolverClient
    @ObservationIgnored private let renew: () async throws -> Void
    @ObservationIgnored private let prepareSession: () async throws -> Void
    @ObservationIgnored private let debounce: Duration
    @ObservationIgnored private let pollInterval: Duration
    @ObservationIgnored private let livePollInterval: Duration
    @ObservationIgnored private let announcer: any RemoteTimerAnnouncer
    @ObservationIgnored private let catchUpHold: Duration
    @ObservationIgnored private var catchUpWaiters: [CheckedContinuation<Void, Never>] = []
    @ObservationIgnored private var catchUpDeadline: Task<Void, Never>?
    /// Attempts started so far, and how many had when the catch-up began:
    /// only an attempt started after that is the catch-up.
    @ObservationIgnored private var attemptsStarted = 0
    @ObservationIgnored private var catchUpAfterAttempt = 0
    @ObservationIgnored private var coordinator: SyncCoordinator!

    /// Set by the coordinator's callbacks during one attempt.
    @ObservationIgnored private var renewedDuringAttempt = false
    @ObservationIgnored private var freshProofRequired = false
    /// Part of the owner's imported history was refused. Outranks "up to
    /// date" on every later sync, so a routine poll cannot paper over it.
    @ObservationIgnored private var importRefused = false
    /// When the current run of network failures began. Any answer from the
    /// server, refusals included, ends the run.
    @ObservationIgnored private var offlineSince: Date?
    @ObservationIgnored private let now: () -> Date
    /// Whether the phone has any usable network path, VPN aside.
    @ObservationIgnored private let internetAvailable: () -> Bool

    /// The one piece of work talking to the server; its value is the error
    /// the work threw, if any.
    @ObservationIgnored private var running: Task<(any Error)?, Never>?
    @ObservationIgnored private var followUp: SyncReason?
    @ObservationIgnored private(set) var scheduledPush: Task<Void, Never>?
    @ObservationIgnored private var polling: Task<Void, Never>?
    /// The interval `polling` runs at, or nil while it does not run.
    @ObservationIgnored private(set) var foregroundPollInterval: Duration?

    /// The hint socket, when the composition gave one.
    @ObservationIgnored private let liveChannel: LiveHintChannel?
    @ObservationIgnored private var live: FamilySyncWebSocket?
    /// `GET /v1/server`, fetched once per engine: the capability and the
    /// instance do not change under a running engine.
    @ObservationIgnored private var liveServerInfo: ServerInfo?
    /// Starts and stops of the socket, chained so a quick background and
    /// foreground cannot reach the actor out of order.
    @ObservationIgnored private var liveControl: Task<Void, Never>?
    /// The highest hint heard while a sync was running. The follow-up it asked
    /// for is skipped if that sync's cursor already covers it — which is the
    /// usual fate of this phone's own hint, sent at commit while its push is
    /// still waiting for the response. `Int64.max` for a reconnect, which no
    /// cursor covers: nothing said what happened while the socket was down.
    @ObservationIgnored private(set) var pendingHintSequence: Int64?
    /// The socket's state, as it last reported it.
    @ObservationIgnored private(set) var liveState: FamilySyncWebSocket.State = .stopped
    /// Set by `stop()` and never cleared: a stopped engine is discarded, and
    /// nothing may reach the server — or the store — through it again.
    @ObservationIgnored private var stopped = false

    /// - Parameters:
    ///   - live: the hint socket. Nil keeps the engine on polling alone.
    ///   - foregroundPollInterval: replaces both foreground poll intervals.
    ///     For the live-server gate, which must prove a change arrived through
    ///     the socket and not through a poll.
    /// - Parameter onRevoked: run after the shared realm is already purged,
    ///   with the realm that was just purged — not `store.activeRealm`, which
    ///   by then has moved back to `.local`. This is the ordinary-sync
    ///   equivalent of the settings screen's own leave/disconnect cleanup: a
    ///   membership revoked while the app merely polls in the background
    ///   never runs through `FamilySettingsViewModel` at all, so anything
    ///   scoped to the family realm — this device's saved invitation codes,
    ///   currently — has to be cleared from here instead.
    public convenience init(
        store: BabyStore,
        familyKey: FamilySessionKey,
        transport: any FamilySyncTransport,
        importClient: any ImportResolverClient,
        renew: @escaping () async throws -> Void,
        prepareSession: @escaping () async throws -> Void = {},
        live: LiveHintChannel? = nil,
        foregroundPollInterval: Duration? = nil,
        diagnostics: (any SyncDiagnosticSink)? = nil,
        now: @escaping () -> Date = Date.init,
        internetAvailable: @escaping () -> Bool = { NetworkPath.shared.isSatisfied },
        onRevoked: @escaping (RealmID) async -> Void = { _ in }
    ) {
        self.init(
            store: store, familyKey: familyKey, transport: transport, importClient: importClient,
            renew: renew, prepareSession: prepareSession, debounce: Self.defaultDebounce,
            pollInterval: foregroundPollInterval ?? Self.defaultPollInterval,
            livePollInterval: foregroundPollInterval ?? Self.defaultLivePollInterval,
            announcer: LiveActivityRemoteTimerAnnouncer(), live: live, diagnostics: diagnostics,
            now: now, internetAvailable: internetAvailable, onRevoked: onRevoked
        )
    }

    /// The schedule is injectable so tests do not wait real seconds.
    init(
        store: BabyStore,
        familyKey: FamilySessionKey,
        transport: any FamilySyncTransport,
        importClient: any ImportResolverClient,
        renew: @escaping () async throws -> Void,
        prepareSession: @escaping () async throws -> Void = {},
        debounce: Duration,
        pollInterval: Duration,
        livePollInterval: Duration = FamilySyncEngine.defaultLivePollInterval,
        catchUpHold: Duration = FamilySyncEngine.defaultCatchUpHold,
        announcer: any RemoteTimerAnnouncer,
        live: LiveHintChannel? = nil,
        diagnostics: (any SyncDiagnosticSink)? = nil,
        now: @escaping () -> Date = Date.init,
        internetAvailable: @escaping () -> Bool = { true },
        onRevoked: @escaping (RealmID) async -> Void = { _ in }
    ) {
        self.store = store
        self.now = now
        self.internetAvailable = internetAvailable
        self.familyKey = familyKey
        self.importClient = importClient
        self.renew = renew
        self.prepareSession = prepareSession
        self.debounce = debounce
        self.pollInterval = pollInterval
        self.livePollInterval = livePollInterval
        self.catchUpHold = catchUpHold
        self.announcer = announcer
        self.liveChannel = live
        coordinator = SyncCoordinator(
            store: store,
            transport: transport,
            accountKey: familyKey.accountSessionKey,
            onRevoked: onRevoked,
            onFreshProofRequired: { [weak self] in self?.freshProofRequired = true },
            onRefreshRequired: { [weak self] in try await self?.renewSession() },
            diagnostics: diagnostics,
            onPhase: { [weak self] phase in
                guard let self, !self.stopped, self.status != .needsSignIn else { return }
                self.operationalStatus.phase = phase
            }
        )
        if (try? store.accountAllowsOrdinarySync(familyKey.leaseKey)) == true {
            operationalStatus = SyncOperationalStatus(activity: .active, signIn: .available, live: .unavailable, phase: .idle)
        }
        // Every callback only records or schedules and returns: the socket
        // reads no further frame, and cannot stop, until it does.
        self.live = live?.makeSocket(LiveHintChannel.Callbacks(
            onHint: { [weak self] cursor in await self?.liveHintArrived(cursor) },
            onConnected: { [weak self] in await self?.liveConnected() },
            onUnauthorized: { [weak self] in await self?.liveRefused() ?? false },
            onStateChange: { [weak self] state in await self?.liveStateChanged(state) }
        ))
    }

    // MARK: Activation

    /// The owner brings everything tracked before the family existed.
    ///
    /// The import keeps local IDs, so nothing the owner already sees changes
    /// identity. Resumable at every phase: the checkpoint ID is derived from
    /// the family and this store, so a relaunch finds the import it was in
    /// the middle of rather than starting a second one, and a finished import
    /// is not touched again. A fresh store (the old one moved aside) imports
    /// under a new ID, whatever it holds, and then pulls the family from 0.
    public func activateAsOwner() async throws {
        guard !stopped, try accountMaySync() else { return }
        try await exclusively { engine in try await engine.ownerActivation() }
    }

    private func ownerActivation() async throws {
        let checkpointID = Self.importCheckpointID(for: familyKey, storeID: try store.importStoreID())
        if (try? store.resumeImport(checkpointID: checkpointID))?.phase != .complete {
            // One coordinator — and so one resolver client — for the whole
            // import: the HTTP client remembers the import it is resolving.
            let importer = ImportCoordinator(store: store, client: importClient, checkpointID: checkpointID)
            do {
                try await renewingOnce {
                    try await importer.startOrResume(realm: self.familyKey.realmID, mode: .preserveLocalIDs)
                }
            } catch {
                status = activationStatus(for: error)
                throw error
            }
        }

        await attempt(.launch)
        try settleImport(checkpointID: checkpointID)
    }

    /// Closes the import only once every imported mutation was *applied*.
    ///
    /// "Nothing left to send" is not enough: a rejected or quarantined row is
    /// terminal too, and closing the import over it would mean the owner's
    /// history silently never reached the family. Left open, the next
    /// activation looks again and the owner is told each time.
    private func settleImport(checkpointID: UUID) throws {
        guard let state = try? store.resumeImport(checkpointID: checkpointID), state.phase == .draining else { return }
        let imported = try store.liveOutboxRows(in: familyKey.realmID).filter {
            (try? $0.decodedMutation().importID) == checkpointID
        }
        importRefused = imported.contains { $0.deliveryState == .superseded || $0.deliveryState == .quarantined }
        if importRefused {
            status = .failed(SyncIssueCopy.importIncomplete)
            return
        }
        guard imported.allSatisfy({ $0.deliveryState == .acknowledged }) else { return }
        try store.markImportComplete(checkpointID: checkpointID)
    }

    /// A joining caregiver starts from the family's records, not its own.
    ///
    /// `setActiveRealm` points at the shared realm without moving a row, so
    /// whatever this phone tracked before joining stays in the local realm —
    /// kept, not uploaded, not merged. The snapshot is taken only until a
    /// cursor exists; after that, re-activating is just a sync.
    public func activateAsJoiner() async throws {
        guard !stopped, try accountMaySync() else { return }
        try await exclusively { engine in try await engine.joinerActivation() }
    }

    private func joinerActivation() async throws {
        let realm = familyKey.realmID
        if store.activeRealm != realm {
            try store.setActiveRealm(realm)
        }
        if try store.syncCursor(in: realm) == nil {
            markSyncing()
            do {
                try await renewingOnce { try await self.coordinator.takeSnapshot() }
            } catch {
                status = activationStatus(for: error)
                throw error
            }
        }
        await attempt(.launch)
    }

    // MARK: Scheduling

    /// One sync, now. Joins whatever is already running — a sync or an
    /// activation — rather than starting alongside it; that work makes one
    /// more pass afterwards so a write that landed mid-flight is not left for
    /// the next poll.
    public func syncNow(_ reason: SyncReason) async {
        guard !stopped, status != .needsSignIn else { return }
        if let running {
            // A hint never replaces a follow-up already queued: a local write
            // or a foreground catch-up pulls anyway, and may also be owed a push.
            if reason != .liveHint || followUp == nil { followUp = reason }
            _ = await running.value
            return
        }
        try? await exclusively { engine in await engine.attempt(reason) }
    }

    /// Runs `work` as the one thing talking to the server, then any follow-up
    /// sync requested meanwhile.
    ///
    /// Activation goes through here as well as sync: a poll or push that
    /// interleaved with the import or the snapshot would apply pages against
    /// a base that is still being built. A failed activation drops the
    /// follow-up — its status is the one worth showing, and a sync against a
    /// half-activated realm has nothing useful to do.
    private func exclusively(_ work: @escaping @MainActor (FamilySyncEngine) async throws -> Void) async throws {
        while let running { _ = await running.value }
        // Checked again after the wait, not only on entry: a caller that
        // queued behind running work may resume after `stop()` returned, and
        // must not start anything against a realm about to be purged.
        guard !stopped else { return }
        if let failure = await launch(work).value { throw failure }
    }

    /// Starts `work` as `running` without waiting for it. The caller has
    /// checked that nothing runs and the engine is not stopped.
    private func launch(_ work: @escaping @MainActor (FamilySyncEngine) async throws -> Void) -> Task<(any Error)?, Never> {
        let task = Task { [weak self] () -> (any Error)? in
            guard let self else { return nil }
            var failure: (any Error)?
            var preparing = true
            var preparedVersion: UUID?
            self.markSyncing()
            self.operationalStatus.phase = .request
            do {
                preparedVersion = try self.store.accountCredentialVersion(for: self.familyKey.leaseKey)
                try await self.prepareSession()
                guard !self.stopped, !Task.isCancelled else {
                    self.running = nil
                    return nil
                }
                preparing = false
                try await work(self)
                var next = self.followUp
                while let current = next, !self.stopped {
                    self.followUp = nil
                    let hintCovered = current == .liveHint && self.pendingHintIsCovered()
                    self.pendingHintSequence = nil
                    if !hintCovered { await self.attempt(current) }
                    next = self.followUp
                }
            } catch {
                self.followUp = nil
                self.pendingHintSequence = nil
                failure = error
                if !self.stopped, !Task.isCancelled {
                    if preparing {
                        let bound = error as? CredentialBoundAuthFailure
                        let version = bound?.credentialVersion ?? preparedVersion
                        let isCurrent = version.map { (try? self.store.accountCredentialIsCurrent($0, for: self.familyKey.leaseKey)) == true } ?? false
                        if isCurrent && (bound?.requiresFreshProof == true || SessionRefresher.requiresFreshProof(error) || error is RefreshRecoveryError) {
                            self.status = .needsSignIn
                            self.operationalStatus.signIn = .required
                        } else if error is URLError {
                            self.status = self.offlineStatus()
                        } else if Self.isLockHeldElsewhere(error) {
                            // Another process is renewing, and nothing was
                            // heard from the server: a failure already
                            // showing stays, with its age.
                            switch self.status {
                            case .offline, .serverUnreachable: break
                            default: self.status = .failed(SyncIssueCopy.retrying)
                            }
                        } else {
                            self.status = .failed(SyncIssueCopy.retrying)
                        }
                    } else if SessionRefresher.requiresFreshProof(error) { self.status = .needsSignIn }
                    else if error is URLError { self.status = self.offlineStatus() }
                    AppLog.sync.error("sync failed \(preparing ? "renewing" : "syncing", privacy: .public): \(SyncLogKind.of(error), privacy: .public); status \(String(describing: self.status), privacy: .public)")
                    // The catch-up waits on for an answer, up to its hold.
                    if !Self.isLockHeldElsewhere(error) {
                        self.finishCatchUp(unanswered: error is URLError)
                    }
                }
                if self.status == .needsSignIn {
                    self.operationalStatus.signIn = .required
                    self.operationalStatus.phase = .idle
                } else if self.operationalStatus.signIn == .available {
                    self.operationalStatus.phase = .retryQueued
                }
            }
            self.running = nil
            return failure
        }
        running = task
        return task
    }

    /// Called after every local write. Pushes once, a second after the last
    /// of a burst, and only if something is actually owed — a local-only
    /// write or an already-delivered one does not wake the radio.
    public func localWriteHappened() {
        guard !stopped, status != .needsSignIn else { return }
        scheduledPush?.cancel()
        let delay = debounce
        scheduledPush = Task { [weak self] in
            try? await Task.sleep(for: delay)
            guard !Task.isCancelled, let self else { return }
            guard (try? self.hasUndeliveredMutations()) == true else { return }
            await self.syncNow(.localWrite)
        }
    }

    /// Polls, and opens the live socket, while the app is in the foreground.
    /// Idempotent.
    ///
    /// The poll starts at the short interval: the socket is only just dialling.
    public func startForegroundPolling() {
        guard !stopped, polling == nil else { return }
        beginCatchUp()
        poll(every: pollInterval)
        startLive()
    }

    /// Returns once the catch-up begun on coming to the foreground has an
    /// answer, or at once if none is under way. Never longer than
    /// `catchUpHold`.
    public func waitForCatchUp() async {
        guard isCatchingUp else { return }
        await withCheckedContinuation { catchUpWaiters.append($0) }
    }

    private func beginCatchUp() {
        guard !isCatchingUp, status != .needsSignIn, store.realmIsShared else { return }
        isCatchingUp = true
        catchUpAfterAttempt = attemptsStarted
        let hold = catchUpHold
        catchUpDeadline = Task { [weak self] in
            try? await Task.sleep(for: hold)
            guard !Task.isCancelled, let self else { return }
            // An answer ends the catch-up before this: still catching up
            // means nothing was heard.
            AppLog.sync.notice("catch-up hold passed without an answer")
            self.finishCatchUp(unanswered: true)
        }
    }

    private nonisolated static func isLockHeldElsewhere(_ error: any Error) -> Bool {
        (error as? SessionRefresher.RefreshError) == .lockUnavailable
    }

    /// The catch-up has its answer, or ran out of time. Unanswered with a
    /// network, the server is out of reach now: the parent is about to act
    /// on what this phone last heard, and three minutes is too late to say so.
    private func finishCatchUp(unanswered: Bool) {
        guard isCatchingUp else { return }
        if unanswered, !stopped, internetAvailable() {
            switch status {
            // A held lock's "retrying" heard nothing from the server; any
            // other failure did, and says more than "out of reach" would.
            case .needsSignIn: break
            case .failed(let message) where message != SyncIssueCopy.retrying: break
            default:
                offlineSince = now().addingTimeInterval(-Self.serverUnreachableAfter)
                status = .serverUnreachable
            }
        }
        isCatchingUp = false
        catchUpDeadline?.cancel()
        catchUpDeadline = nil
        let waiters = catchUpWaiters
        catchUpWaiters = []
        for waiter in waiters { waiter.resume() }
    }

    /// Closes the socket with 1000 too, which frees the server's single
    /// socket slot for this device at once rather than after its ping timeout.
    public func stopForegroundPolling() {
        finishCatchUp(unanswered: false)
        polling?.cancel()
        polling = nil
        foregroundPollInterval = nil
        stopLive()
    }

    /// (Re)starts the poll. Kept running even while the socket is up, at the
    /// slow interval: `isInForeground` is read from it.
    private func poll(every interval: Duration) {
        polling?.cancel()
        foregroundPollInterval = interval
        polling = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(for: interval)
                guard !Task.isCancelled, let self else { return }
                await self.syncNow(.foreground)
            }
        }
    }

    /// Winds the engine down for good, before its family is left or purged.
    ///
    /// Returns only once nothing is in flight: a sync still awaiting the
    /// server when the shared realm is purged would otherwise land its pages
    /// and cursor back in the realm that was just emptied. The running work is
    /// cancelled rather than merely awaited, so a slow or unreachable server
    /// does not hold the parent's Disconnect button for a full timeout.
    ///
    /// The socket goes first, and is waited for, so no hint can queue work
    /// behind the drain below. The running work is cancelled before that
    /// wait: the socket may itself be waiting on it, for the sync it runs
    /// when the server refuses its token.
    public func stop() async {
        stopped = true
        finishCatchUp(unanswered: false)
        operationalStatus = .inactive
        coordinator.stopDiagnostics()
        polling?.cancel()
        polling = nil
        foregroundPollInterval = nil
        scheduledPush?.cancel()
        scheduledPush = nil
        followUp = nil
        pendingHintSequence = nil
        running?.cancel()
        if let live {
            let previous = liveControl
            previous?.cancel()
            liveControl = nil
            // A start still under way would otherwise land after this stop.
            await previous?.value
            await live.stop()
        }
        while let running {
            running.cancel()
            _ = await running.value
        }
    }

    // MARK: Live hints

    /// What sync has to say about the live socket, for tests.
    var lastSyncReason: SyncReason? { coordinator.lastReason }
    /// Whether anything is talking to the server right now.
    var isBusy: Bool { running != nil }
    /// The sync owed after the running one, for tests.
    var queuedFollowUp: SyncReason? { followUp }

    /// A hint from the other phone — or this one's own. Returns at once: it
    /// only starts a sync, or records one as owed after the running one.
    func liveHintArrived(_ cursor: Cursor) {
        guard !stopped, !cursorCovers(cursor) else { return }
        coordinator.recordHintReceived()
        requestLiveSync(through: cursor.sequence)
    }

    /// Nothing announced what changed while the socket was down, so a new
    /// connection is itself a reason to sync, and no cursor can excuse it.
    func liveConnected() {
        guard !stopped else { return }
        requestLiveSync(through: .max)
    }

    /// The server refused the socket's token or authorization. One HTTP sync
    /// renews the session or surfaces why — revoked, epoch changed — through
    /// the paths sync already has. Dialling on is pointless only once the
    /// parent has to sign in.
    ///
    /// Owed like a reconnect, so no cursor can excuse it: a hint follow-up
    /// already queued, and covered, would otherwise be skipped, the session
    /// never renewed, and the socket redial with the same refused token.
    func liveRefused() async -> Bool {
        guard !stopped else { return false }
        operationalStatus.live = .refused
        requestLiveSync(through: .max)
        if let running { _ = await running.value }
        return !stopped && status != .needsSignIn
    }

    /// The poll slows while the socket is up and speeds up again the moment
    /// it is not.
    private func liveStateChanged(_ state: FamilySyncWebSocket.State) {
        liveState = state
        guard !stopped else { return }
        switch state {
        case .stopped: operationalStatus.live = .unavailable
        case .connecting: operationalStatus.live = .dialing
        case .connected: operationalStatus.live = .connected
        case .backingOff: operationalStatus.live = .reconnecting
        }
        guard !stopped, polling != nil else { return }
        let wanted = state == .connected ? livePollInterval : pollInterval
        if wanted != foregroundPollInterval { poll(every: wanted) }
    }

    private func requestLiveSync(through sequence: Int64) {
        guard !stopped, status != .needsSignIn else { return }
        guard running == nil else {
            pendingHintSequence = max(pendingHintSequence ?? sequence, sequence)
            if followUp == nil { followUp = .liveHint }
            return
        }
        _ = launch { engine in await engine.attempt(.liveHint) }
    }

    /// The local cursor already includes this hint: same server instance,
    /// epoch and family, at or past its sequence.
    private func cursorCovers(_ hint: Cursor) -> Bool {
        guard let local = try? store.syncCursor(in: familyKey.realmID) else { return false }
        return local.serverInstanceID == hint.serverInstanceID
            && local.recoveryEpoch == hint.recoveryEpoch
            && local.familyID == hint.familyID
            && local.sequence >= hint.sequence
    }

    /// A hint follow-up has nothing to do when the pass before it already
    /// pulled past the hint and nothing is owed to the server.
    private func pendingHintIsCovered() -> Bool {
        guard let pending = pendingHintSequence,
              let local = try? store.syncCursor(in: familyKey.realmID), local.sequence >= pending
        else { return false }
        return (try? hasUndeliveredMutations()) == false
    }

    private func startLive() {
        guard let live, let channel = liveChannel else { return }
        operationalStatus.live = .dialing
        let previous = liveControl
        let realm = familyKey.realmID
        liveControl = Task { [weak self] in
            await previous?.value
            guard let self, let info = await self.probeServer(channel) else { return }
            // Only this realm's server. A different instance at the same
            // origin is not the family's server, whatever it advertises.
            guard !Task.isCancelled, !self.stopped, self.isInForeground else { return }
            guard info.serverInstanceID == self.familyKey.serverInstanceID else {
                self.operationalStatus.live = .unavailable
                return
            }
            if !(await live.start(realm: realm, serverInfo: info)), !self.stopped {
                self.operationalStatus.live = .unavailable
            }
        }
    }

    private func stopLive() {
        guard let live else { return }
        operationalStatus.live = .unavailable
        let previous = liveControl
        previous?.cancel()
        liveControl = Task {
            await previous?.value
            await live.stop()
        }
    }

    /// The server's capabilities, once per engine. A failed probe is backed
    /// off and retried, not taken as "no live hints": a flaky network at
    /// launch must not leave the family polling for the whole session.
    private func probeServer(_ channel: LiveHintChannel) async -> ServerInfo? {
        if let liveServerInfo { return liveServerInfo }
        var attempt = 0
        while !Task.isCancelled, !stopped {
            do {
                let info = try await channel.serverInfo()
                liveServerInfo = info
                return info
            } catch {
                attempt += 1
                do { try await channel.sleep(channel.probeRetry.delay(forAttempt: attempt)) } catch { return nil }
            }
        }
        return nil
    }

    // MARK: Internals

    /// One scheduled sync: as many passes as it takes to empty the outbox,
    /// because the coordinator sends at most one batch per pass and an
    /// owner's imported history is usually more than one batch.
    private func attempt(_ reason: SyncReason) async {
        attemptsStarted += 1
        let isCatchUp = attemptsStarted > catchUpAfterAttempt
        var unanswered = false
        defer { if isCatchUp { finishCatchUp(unanswered: unanswered) } }
        // A fresh Apple proof is the only way out of this state. Keep its
        // button visible and avoid repeatedly sending the same refused token.
        guard !stopped, status != .needsSignIn, (try? accountMaySync()) == true else { return }
        // Not pointed at the family: there is nothing to sync, and the last
        // success says nothing about now.
        guard store.realmIsShared else {
            switch status {
            case .upToDate, .syncing, .offline, .serverUnreachable: status = .idle
            default: break
            }
            return
        }
        markSyncing()
        operationalStatus.phase = .request
        var owed = (try? store.nextDispatchBatch(limit: Int.max).count) ?? 0
        for _ in 0..<Self.maximumPassesPerSync {
            if stopped { break }
            renewedDuringAttempt = false
            freshProofRequired = false
            await coordinator.syncOnce(reason: reason)
            // A renewed session gets exactly one retry of the same pass; a
            // second expiry straight after a successful renewal is not
            // something another renewal will fix.
            if renewedDuringAttempt, !coordinator.lastAttemptSucceeded {
                await coordinator.syncOnce(reason: reason)
            }
            guard coordinator.lastAttemptSucceeded else { break }
            let remaining = (try? store.nextDispatchBatch(limit: Int.max).count) ?? 0
            guard remaining > 0, remaining < owed else { break }
            owed = remaining
        }
        guard !stopped else { return }
        status = currentStatus()
        unanswered = coordinator.isOffline
        if !coordinator.lastAttemptSucceeded {
            AppLog.sync.error("sync attempt failed: \(self.coordinator.lastFailureKind ?? "unknown", privacy: .public); status \(String(describing: self.status), privacy: .public)")
        }
        if status == .needsSignIn {
            operationalStatus.signIn = .required
            operationalStatus.phase = .idle
        } else {
            operationalStatus.phase = coordinator.lastAttemptSucceeded ? .idle : .retryQueued
        }
        await announceRemoteTimers()
        // Only after a success: a failed attempt leaves the store no newer
        // than the cards the server pushed, which must not be overwritten.
        if coordinator.lastAttemptSucceeded, !stopped {
            await announcer.syncSucceeded(in: store)
        }
    }

    /// Whether the app is on screen. Foreground polling runs exactly while it
    /// is, so that is the signal — there is no second flag to keep in step.
    var isInForeground: Bool { polling != nil }

    /// Tells the Lock Screen what another caregiver's timers did during this
    /// sync.
    ///
    /// Here, once the whole attempt has landed, rather than inside remote
    /// apply: apply runs inside the store's write boundary and once per page
    /// or snapshot page, and a nap started and stopped within one sync would
    /// otherwise flash a card up and down. A new card is only requested in
    /// the foreground — ActivityKit refuses one from the background, and the
    /// timer is on Home either way — but an ending always goes out, so no card
    /// outlives its timer.
    private func announceRemoteTimers() async {
        for transition in store.drainRemoteTimerTransitions() {
            switch transition {
            case .appeared(let timerID):
                guard isInForeground, let timer = try? store.activeTimer(id: timerID) else { continue }
                await announcer.started(timer, in: store)
            case .changed(let timerID):
                guard let timer = try? store.activeTimer(id: timerID) else { continue }
                await announcer.changed(timer, in: store)
            case .ended(let timerID, let liveActivityID):
                await announcer.ended(timerID: timerID, liveActivityID: liveActivityID, in: store)
            }
        }
    }

    /// A retry after a failure keeps the failure showing until it has an
    /// answer. Flipping to "Syncing" for every retry made Home's offline card
    /// blink out and back every 20 seconds, shifting everything under it.
    private func markSyncing() {
        switch status {
        case .offline, .serverUnreachable: break
        default: status = .syncing
        }
    }

    /// "Offline", until the failures have outlasted `serverUnreachableAfter`
    /// on a phone that has a network: then the server is what is missing.
    /// Only failures with a network count: a phone back from hours in
    /// airplane mode starts the clock when the network returns.
    private func offlineStatus() -> Status {
        guard internetAvailable() else {
            offlineSince = nil
            return .offline
        }
        let at = now()
        let since = offlineSince ?? at
        offlineSince = since
        return at.timeIntervalSince(since) >= Self.serverUnreachableAfter ? .serverUnreachable : .offline
    }

    private func currentStatus() -> Status {
        if freshProofRequired { return .needsSignIn }
        if let issue = coordinator.currentIssue { return .failed(issue.presentation.message) }
        if coordinator.isOffline { return offlineStatus() }
        if coordinator.lastAttemptSucceeded {
            return importRefused ? .failed(SyncIssueCopy.importIncomplete) : .upToDate(Date())
        }
        // Backing off from a transient server answer: nothing is wrong with
        // the entries and the next poll retries.
        return .failed(SyncIssueCopy.retrying)
    }

    /// Runs one activation step, renewing the session and trying once more
    /// if the server says the access token will not do.
    ///
    /// The first activation after creating or joining always needs this: the
    /// device still holds the session it had before it was in a family, and
    /// the import and snapshot routes answer `access_expired` to anything that
    /// is not family-scoped (`httpapi/import.go`, `httpapi/sync.go`). The
    /// server's identity protocol has the client rotate that session once
    /// through refresh; the sync path already renewed on its own, but import
    /// and snapshot run before the first sync and never did — so neither an
    /// owner's history nor a joiner's snapshot ever arrived. It also covers a
    /// relaunch after the 10-minute access token lapsed mid-import.
    ///
    /// Once, not in a loop: a session refused again straight after renewal is
    /// not something another renewal will fix.
    private func renewingOnce(_ step: () async throws -> Void) async throws {
        do {
            try await step()
        } catch SyncErrorCode.accessExpired {
            try await renew()
            try await step()
        } catch let failure as CredentialBoundAuthFailure {
            guard (try? store.accountCredentialIsCurrent(failure.credentialVersion, for: familyKey.leaseKey)) == true,
                  !failure.requiresFreshProof,
                  case .status(401, "access_expired") = failure.underlying else { throw failure }
            try await renew()
            try await step()
        }
    }

    private func renewSession() async throws {
        try await renew()
        renewedDuringAttempt = true
    }

    /// The durable gate, read the same way the coordinator reads it. Checked
    /// here too so a closed gate shows as "sign in" rather than as a sync
    /// that silently did nothing and claimed to be up to date.
    private func accountMaySync() throws -> Bool {
        guard try store.accountAllowsOrdinarySync(familyKey.leaseKey) else {
            status = .needsSignIn
            operationalStatus.signIn = .required
            return false
        }
        operationalStatus.activity = .active
        operationalStatus.signIn = .available
        return true
    }

    private func hasUndeliveredMutations() throws -> Bool {
        try !store.nextDispatchBatch(limit: 1).isEmpty
    }

    /// What an activation failure means to a parent. Never the raw error:
    /// its description is for a log, not for the screen.
    private func activationStatus(for error: any Error) -> Status {
        if let failure = error as? CredentialBoundAuthFailure {
            guard (try? store.accountCredentialIsCurrent(failure.credentialVersion, for: familyKey.leaseKey)) == true else {
                return (try? store.accountAllowsOrdinarySync(familyKey.leaseKey)) == false
                    ? .needsSignIn : .failed(SyncIssueCopy.retrying)
            }
            if failure.requiresFreshProof { return .needsSignIn }
            if case .status(_, let code) = failure.underlying {
                return activationStatus(for: SyncErrorCode(rawValue: code ?? "") ?? .serviceUnavailable)
            }
            return .failed(SyncIssueCopy.retrying)
        }
        if error is URLError { return offlineStatus() }
        // A renewal inside the activation that only Apple can put right.
        if SessionRefresher.requiresFreshProof(error) { return .needsSignIn }
        // One that clears by itself — another process renewing — is the
        // ordinary backoff, not a setup failure.
        if error is SessionRefresher.RefreshError { return .failed(SyncIssueCopy.retrying) }
        if let code = error as? SyncErrorCode {
            if code.action == .freshProofRequired { return .needsSignIn }
            return .failed(SyncIssueStore.issue(for: code, realm: familyKey.realmID)?.presentation.message
                ?? SyncIssueCopy.retrying)
        }
        if case ImportCoordinatorError.mappingIncomplete = error { return .failed(SyncIssueCopy.importConflict) }
        return .failed(SyncIssueCopy.familySetupIncomplete)
    }

    /// Derived: the same family, device and store always name the same
    /// import, so resuming after a crash needs nothing but the key and the
    /// store's own `importStoreID`.
    ///
    /// The store is part of the name because the server never forgets an
    /// import ID. A store moved aside keeps the device (the defaults and the
    /// Keychain survive, and signing in again proves continuity), so without
    /// it the fresh store re-derived the import its predecessor finished, and
    /// the server refused the new first chunk (`import_chunk_reused`) on every
    /// activation. The owner then never pulled the family at all.
    ///
    /// The recovery epoch is deliberately left out — it changes on session
    /// renewal paths, and a new epoch must resume the import, not start a
    /// second one that would upload the owner's history twice.
    static func importCheckpointID(for key: FamilySessionKey, storeID: UUID) -> UUID {
        let seed = [
            "family-import-v2",
            key.canonicalOrigin,
            key.serverInstanceID.uuidString.lowercased(),
            key.familyID.uuidString.lowercased(),
            key.accountID.uuidString.lowercased(),
            key.deviceID.uuidString.lowercased(),
            storeID.uuidString.lowercased(),
        ].joined(separator: "|")
        var bytes = Array(SHA256.hash(data: Data(seed.utf8)).prefix(16))
        bytes[6] = (bytes[6] & 0x0F) | 0x50 // version 5 (name-based, SHA)
        bytes[8] = (bytes[8] & 0x3F) | 0x80 // RFC 4122 variant
        return UUID(uuid: (
            bytes[0], bytes[1], bytes[2], bytes[3], bytes[4], bytes[5], bytes[6], bytes[7],
            bytes[8], bytes[9], bytes[10], bytes[11], bytes[12], bytes[13], bytes[14], bytes[15]
        ))
    }
}

/// How an engine reaches the live hint socket: the server probe that gates
/// it, and the socket itself, built once the engine can hand it callbacks.
public struct LiveHintChannel: Sendable {
    /// What the socket reports, bound to the engine that owns it.
    public struct Callbacks: Sendable {
        public let onHint: @Sendable (Cursor) async -> Void
        public let onConnected: @Sendable () async -> Void
        public let onUnauthorized: @Sendable () async -> Bool
        public let onStateChange: @Sendable (FamilySyncWebSocket.State) async -> Void
    }

    let serverInfo: @Sendable () async throws -> ServerInfo
    let makeSocket: @Sendable (Callbacks) -> FamilySyncWebSocket
    let probeRetry: SyncRetryPolicy
    let sleep: @Sendable (TimeInterval) async throws -> Void

    public init(
        serverInfo: @escaping @Sendable () async throws -> ServerInfo,
        makeSocket: @escaping @Sendable (Callbacks) -> FamilySyncWebSocket
    ) {
        self.init(
            serverInfo: serverInfo,
            probeRetry: SyncRetryPolicy(base: 1, ceiling: 60, maximumAttempts: .max),
            sleep: { try await Task.sleep(for: .seconds($0)) },
            makeSocket: makeSocket
        )
    }

    init(
        serverInfo: @escaping @Sendable () async throws -> ServerInfo,
        probeRetry: SyncRetryPolicy,
        sleep: @escaping @Sendable (TimeInterval) async throws -> Void,
        makeSocket: @escaping @Sendable (Callbacks) -> FamilySyncWebSocket
    ) {
        self.serverInfo = serverInfo
        self.probeRetry = probeRetry
        self.sleep = sleep
        self.makeSocket = makeSocket
    }
}
