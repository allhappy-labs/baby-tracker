import BabyCore
import BabyProtocol
import BabyStore
import Foundation

/// Gives registration work time to finish when it starts in the background —
/// a push-to-start wake, a rotated token — and lets it be bounded.
public protocol LiveActivityPushBackgroundExecution: Sendable {
    @MainActor func perform(_ work: @escaping @MainActor () async -> Void) async
}

/// No extra time: the work runs for as long as the process does anyway.
public struct InlineLiveActivityPushExecution: LiveActivityPushBackgroundExecution {
    public init() {}
    @MainActor public func perform(_ work: @escaping @MainActor () async -> Void) async { await work() }
}

/// Registers this device's ActivityKit push tokens with the built-in server.
///
/// One per process, owned from app launch rather than any view, so a
/// background launch caused by a remote start registers its update token with
/// no scene at all. ActivityKit events are observed for the life of the
/// process; a `LiveActivityPushContext` decides whether, and where, they go.
///
/// Work is a single serial pump per context: every trigger (launch, foreground,
/// a token, a state change) only marks the desired state dirty, and each pass
/// reconciles that state with the ledger and the server. Versions come from
/// the App Group ledger, so they are monotonic across relaunches and
/// processes. A failed pass retries after 2, 5 and 15 seconds, then waits for
/// the next trigger instead of looping in the background.
///
/// A realm change cancels the old pump. Every step re-checks, on the main
/// actor and with no await in between, that its context is still current
/// before it records a receipt or sends anything else, so an old task cannot
/// upload into — or write state for — the new realm.
@MainActor
public final class LiveActivityPushCoordinator {
    public typealias ClientFactory = @MainActor (LiveActivityPushContext) -> any LiveActivityPushRegistering

    public static let retryDelays: [Duration] = [.seconds(2), .seconds(5), .seconds(15)]
    /// The server accepts at most this many absence proofs per inventory.
    static let maximumAbsenceProofs = 16

    let events: any LiveActivityPushEventSource
    let store: LiveActivityRegistrationStore
    private let makeClient: ClientFactory
    private let retryDelays: [Duration]
    private let sleep: @Sendable (Duration) async throws -> Void
    private let background: any LiveActivityPushBackgroundExecution
    /// Whether the app is in the foreground right now. Only then may
    /// `Activity.activities` be read as an absence proof.
    private let isForeground: @MainActor () -> Bool
    /// Where the card controller learns which realm is push-managed and which
    /// cards the server holds; it hands back the duplicates it retires.
    let pushState: LiveActivityPushState
    /// Cards the controller retired: never registered again.
    var retired: Set<String> = []

    /// The current context; nil when push is inert for this process.
    public private(set) var context: LiveActivityPushContext?
    /// Called when ActivityKit reports something while no context is
    /// installed, so the owner can decide whether to evaluate again.
    public var onEventWithoutSession: (@MainActor () -> Void)?
    /// What ActivityKit last said, independent of any context.
    private(set) var pushToStartToken: String?
    var observed: [String: LiveActivityObservation] = [:]
    /// The user's latest Settings answer, once one arrives after `start`.
    private var activitiesEnabledOverride: Bool?

    private var session: Session?
    private var eventTask: Task<Void, Never>?
    private var pump: Task<Void, Never>?
    private var pumpToken: UUID?
    private var startTicket = 0
    private var dirty = false
    /// Re-send an accepted device registration: launch, foreground, reconcile.
    /// Cleared only by a successful send, so a failed replay is retried.
    var replayDevice = false
    /// A foreground caller asked for an inventory. Taken by the very next
    /// pass and nowhere else: a retry seconds later may run in the background.
    private var foregroundPass = false

    final class Session {
        var context: LiveActivityPushContext
        let client: any LiveActivityPushRegistering
        let installationID: UUID
        /// The server accepted this device's registration in this realm.
        var accepted = false
        init(context: LiveActivityPushContext, client: any LiveActivityPushRegistering, installationID: UUID) {
            self.context = context
            self.client = client
            self.installationID = installationID
        }
    }

    enum Outcome: Equatable { case idle, retry, failClosed, deviceStale }

    public init(
        events: any LiveActivityPushEventSource,
        store: LiveActivityRegistrationStore,
        makeClient: @escaping ClientFactory,
        retryDelays: [Duration] = LiveActivityPushCoordinator.retryDelays,
        sleep: @escaping @Sendable (Duration) async throws -> Void = { try await Task.sleep(for: $0) },
        background: any LiveActivityPushBackgroundExecution = InlineLiveActivityPushExecution(),
        isForeground: @escaping @MainActor () -> Bool = { false },
        pushState: LiveActivityPushState = LiveActivityPushState()
    ) {
        self.events = events
        self.store = store
        self.makeClient = makeClient
        self.retryDelays = retryDelays
        self.sleep = sleep
        self.background = background
        self.isForeground = isForeground
        self.pushState = pushState
        retired = Set((try? store.read().retiredActivityIDs) ?? [])
        // From the first instant of a cold launch: the realm the server last
        // accepted this device for, until a probe answers definitively.
        LiveActivityPushCardState.installProvisional(from: store, into: pushState)
        pushState.setRetireHandler { [weak self] ids in self?.retire(ids) ?? false }
        // A card the app requested shows a lifecycle only the controller
        // knows; a change to it is a newer registration of that card.
        pushState.setReferencesHandler { [weak self] in
            guard let self, self.session != nil else { return }
            self.kick()
        }
    }

    // MARK: Lifecycle

    /// Subscribes to ActivityKit once per process. Tokens that arrive before
    /// any context are kept in memory and registered once one exists.
    public func observeEvents() {
        guard eventTask == nil else { return }
        let stream = events.events()
        eventTask = Task { [weak self] in
            for await event in stream {
                guard let self else { return }
                self.handle(event)
                self.eventHandled()
            }
        }
    }

    // MARK: Synchronisation seam

    /// How many ActivityKit events have been taken off the stream and handled.
    private(set) var handledEventCount = 0
    private var handledEventWaiters: [(count: Int, continuation: CheckedContinuation<Void, Never>)] = []

    /// Returns once `count` events have been handled. Lets a caller (the
    /// suites) wait for event delivery deterministically instead of polling.
    func waitForHandledEvents(_ count: Int) async {
        guard handledEventCount < count else { return }
        await withCheckedContinuation { handledEventWaiters.append((count, $0)) }
    }

    private func eventHandled() {
        handledEventCount += 1
        let ready = handledEventWaiters.filter { $0.count <= handledEventCount }
        handledEventWaiters.removeAll { $0.count <= handledEventCount }
        for waiter in ready { waiter.continuation.resume() }
    }

    /// Installs `context`. The same realm (family session, endpoint, APNs
    /// environment) is updated in place and replayed; any other realm first
    /// cancels and joins the old session's work.
    public func start(context: LiveActivityPushContext) async {
        observeEvents()
        startTicket += 1
        let ticket = startTicket
        activitiesEnabledOverride = nil
        if let session, session.context.realm == context.realm {
            session.context = context
            self.context = context
            replayDevice = true
            publishCards()
            kick()
            return
        }
        await cancelSession()
        guard ticket == startTicket else { return }
        let installation: UUID
        do {
            installation = try store.bind(LiveActivityRegistrationIdentity(familyKey: context.familyKey))
        } catch {
            // Fail closed: without a trustworthy ledger, nothing is versioned.
            AppLog.sync.error("live activity registration ledger unavailable")
            return
        }
        session = Session(context: context, client: makeClient(context), installationID: installation)
        self.context = context
        retired = Set((try? store.read().retiredActivityIDs) ?? [])
        replayDevice = true
        publishCards()
        kick()
    }

    /// Ends the session: no further request, receipt or ledger write from it.
    ///
    /// `withdrawingPush`: a definitive no — another server, no family, a
    /// sign-out — hands the cards back to the app. False when
    /// nothing is known yet (a probe that got no answer), so a pushed card is
    /// not mistaken for a local one.
    public func stop(withdrawingPush: Bool = true) async {
        startTicket += 1
        await cancelSession()
        if withdrawingPush { pushState.withdraw() }
    }

    /// Replays the registration (launch, a background trigger) and returns
    /// once the pump is idle or has deferred.
    public func reconcile() async {
        replayDevice = true
        kick()
        await waitUntilIdle()
    }

    /// The same from the foreground, which alone may read `Activity.activities`
    /// as proof that an unconfirmed remote start is absent.
    public func reconcileForeground() async {
        replayDevice = true
        foregroundPass = true
        kick()
        await waitUntilIdle()
    }

    /// The user's feed break limit changed: not a realm change, a new version.
    public func updateFeedBreakMinutes(_ minutes: Int) {
        guard let session, session.context.feedBreakMinutes != minutes else { return }
        session.context = session.context.replacing(feedBreakMinutes: minutes)
        context = session.context
        kick()
    }

    func waitUntilIdle() async {
        while let running = pump {
            await running.value
            if pump == running { return }
        }
    }

    private func cancelSession() async {
        session = nil
        context = nil
        foregroundPass = false
        // Between realms nothing is known; `stop` withdraws definitively.
        pushState.install(nil)
        let running = pump
        pump = nil
        pumpToken = nil
        running?.cancel()
        await running?.value
    }

    // MARK: Events

    private func handle(_ event: LiveActivityPushEvent) {
        switch event {
        case .pushToStartToken(let token):
            guard Self.isToken(token), token != pushToStartToken else { return }
            pushToStartToken = token
        case .activity(var observation):
            // A duplicate the card controller retired is nobody's business
            // any more, whatever ActivityKit still says about it.
            guard !retired.contains(observation.activityID) else { return }
            // An unknown state is ignored: the card keeps the last state this
            // build understood.
            if observation.state == .unknown, let known = observed[observation.activityID]?.state { observation.state = known }
            if observation.pushToken.map(Self.isToken) == false { observation.pushToken = nil }
            if observation.pushToken == nil { observation.pushToken = observed[observation.activityID]?.pushToken }
            guard observed[observation.activityID] != observation else { return }
            observed[observation.activityID] = observation
        case .activitiesEnabled(let enabled):
            guard activitiesEnabledOverride != enabled else { return }
            activitiesEnabledOverride = enabled
            replayDevice = true
        }
        guard session != nil else {
            onEventWithoutSession?()
            return
        }
        publishCards()
        kick()
    }

    // MARK: Cards

    /// Tells the card controller which realm the server pushes to, which
    /// cards it may hold, and which lifecycles were dismissed.
    private func publishCards() {
        guard let session else { return }
        guard let ledger = try? store.read() else {
            pushState.install(nil)
            return
        }
        let (publication, accepted) = LiveActivityPushCardState.publication(
            context: session.context, installationID: session.installationID, ledger: ledger, observed: observed,
            acceptedBefore: session.accepted, activitiesEnabled: activitiesEnabledOverride ?? session.context.activitiesAllowed
        )
        session.accepted = accepted
        switch publication.mode {
        case .withdrawn: pushState.withdraw()
        case .unknown: pushState.install(nil)
        case .managed(let fence): pushState.install(fence)
        }
        pushState.publish(registrations: publication.registrations, dismissedLifecycles: publication.dismissed)
    }

    /// Cards the card controller is about to end that the server must not
    /// read as dismissed: recorded in the ledger first, then forgotten here.
    /// False — and nothing forgotten — when the ledger refuses the write, so
    /// the controller leaves the card up.
    func retire(_ ids: [String]) -> Bool {
        do {
            try LiveActivityPushCardState.retire(ids, in: store)
        } catch {
            AppLog.sync.error("live activity retirement not recorded")
            return false
        }
        retired.formUnion(ids)
        for id in ids { observed[id] = nil }
        publishCards()
        return true
    }

    private static func isToken(_ token: String) -> Bool {
        (try? LiveActivityDeviceRegistration(installationID: UUID(), registrationVersion: 1, apnsEnvironment: .sandbox, pushToStartToken: token)) != nil
    }

    // MARK: Pump

    private func kick() {
        dirty = true
        guard pump == nil, let session else { return }
        let token = UUID()
        pumpToken = token
        pump = Task { [weak self] in
            while let self {
                await self.background.perform { [weak self] in
                    await self?.drain(session)
                }
                guard self.pumpToken == token else { return }
                // A trigger that landed while the drain was returning would
                // otherwise find this pump still set and be dropped.
                if self.dirty, self.session === session, !Task.isCancelled { continue }
                self.pump = nil
                self.pumpToken = nil
                return
            }
        }
    }

    func isCurrent(_ session: Session) -> Bool {
        self.session === session && !Task.isCancelled
    }

    private func drain(_ session: Session) async {
        var attempt = 0
        while isCurrent(session) {
            dirty = false
            let outcome = await pass(session)
            guard isCurrent(session) else { return }
            publishCards()
            switch outcome {
            case .idle:
                guard dirty else { return }
            case .retry:
                // Bounded: once the schedule is spent, the next launch,
                // foreground or token event starts a fresh one.
                guard attempt < retryDelays.count else { return }
                do { try await sleep(retryDelays[attempt]) } catch { return }
                attempt += 1
            case .failClosed, .deviceStale:
                return
            }
        }
    }

    private func pass(_ session: Session) async -> Outcome {
        let foregroundRequested = foregroundPass
        foregroundPass = false
        let removals = await sendRemovals(session)
        guard removals == .idle, isCurrent(session) else { return removals }
        guard activitiesEnabledOverride ?? session.context.activitiesAllowed else {
            guard enqueueRevocation(session) else { return .idle }
            return await sendRemovals(session)
        }
        // A server not advertising push takes down, never up: no device,
        // inventory or card goes to it until a probe finds the capability back.
        guard session.context.pushAdvertised else { return .idle }

        var outcome = await syncDevice(session)
        guard outcome == .idle, isCurrent(session) else { return outcome }
        // Read only if this pass was the one asked for, and only if the app
        // is still in the foreground at the moment of reading.
        if foregroundRequested, isForeground() {
            let present = events.inventory()
            reconcileDismissals(session, present: present)
            if prepareForegroundInventory(session, present: present) {
                outcome = await syncDevice(session)
                guard outcome == .idle, isCurrent(session) else { return outcome }
            }
        }
        outcome = await syncActivities(session)
        if outcome == .deviceStale, isCurrent(session) {
            // The server no longer holds this device's row for the current
            // session lineage: a newer device version, then the cards once.
            // A card refused again after that is refused for itself: it waits
            // for the next trigger and the others still register.
            outcome = await syncDevice(session)
            guard outcome == .idle, isCurrent(session) else { return outcome }
            outcome = await syncActivities(session, skippingDenied: true)
        }
        return outcome
    }
    func outcome(of error: any Error) -> Outcome {
        switch error {
        case is CancellationError: return .idle
        case let error as LiveActivityPushClientError where error.isRetryable: return .retry
        case is LiveActivityPushClientError: return .failClosed
        default: return .retry
        }
    }
}
