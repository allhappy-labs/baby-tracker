import BabyCore
import BabyProtocol
import Foundation

/// Turns this process's restored account state into a push context, with no
/// settings screen, no prompt, and no dependency on a scene.
///
/// Started from app launch. It re-evaluates when the family mode changes, when
/// the app becomes active, and when the feed break preference changes; each
/// evaluation probes the server for the capability before installing a
/// context. Anything other than the built-in server — or a build with no APNs
/// environment — never probes and never registers. The built-in server
/// without the capability gets a take-down-only context: the removals this
/// installation owes it, nothing else.
@MainActor
public final class LiveActivityPushDriver {
    public let coordinator: LiveActivityPushCoordinator
    private let builtIn: ConfirmedEndpoint?
    private let mode: @MainActor () -> FamilySyncMode
    private let endpoint: @MainActor () -> ConfirmedEndpoint?
    private let probe: @Sendable (ServerOrigin) async throws -> ServerInfo
    private let activitiesAllowed: @Sendable () -> Bool
    private let environment: any LiveActivityAPNsEnvironmentResolving
    private let feedBreakMinutes: @MainActor () -> Int
    private let background: any LiveActivityPushBackgroundExecution
    private let retryDelays: [Duration]
    private let sleep: @Sendable (Duration) async throws -> Void
    private var evaluation = 0
    /// Evaluations in progress, including their retry waits.
    private var evaluating = 0
    /// The last evaluation ended without any answer from the server (network,
    /// TLS) and without a session: the next ActivityKit event re-evaluates.
    private var probeUnanswered = false
    /// Mode changes before launch (the session restore itself) are covered by
    /// launch's own evaluation.
    private var launched = false
    private var observers: [any NSObjectProtocol] = []
    private var observedCenter: NotificationCenter?

    public init(
        coordinator: LiveActivityPushCoordinator,
        builtIn: ConfirmedEndpoint?,
        mode: @escaping @MainActor () -> FamilySyncMode,
        endpoint: @escaping @MainActor () -> ConfirmedEndpoint?,
        probe: @escaping @Sendable (ServerOrigin) async throws -> ServerInfo,
        activitiesAllowed: @escaping @Sendable () -> Bool,
        environment: any LiveActivityAPNsEnvironmentResolving,
        feedBreakMinutes: @escaping @MainActor () -> Int,
        background: any LiveActivityPushBackgroundExecution = InlineLiveActivityPushExecution(),
        retryDelays: [Duration] = LiveActivityPushCoordinator.retryDelays,
        sleep: @escaping @Sendable (Duration) async throws -> Void = { try await Task.sleep(for: $0) }
    ) {
        self.retryDelays = retryDelays
        self.sleep = sleep
        defer {
            coordinator.onEventWithoutSession = { [weak self] in self?.eventArrivedWithoutSession() }
        }
        self.coordinator = coordinator
        self.builtIn = builtIn
        self.mode = mode
        self.endpoint = endpoint
        self.probe = probe
        self.activitiesAllowed = activitiesAllowed
        self.environment = environment
        self.feedBreakMinutes = feedBreakMinutes
        self.background = background
    }

    /// App launch, foreground or background. ActivityKit is observed from
    /// here on, whatever the evaluation decides.
    public func launch() async {
        launched = true
        coordinator.observeEvents()
        await background.perform { [weak self] in await self?.evaluate(foreground: false) }
    }

    /// The account coordinator's mode changed: a sign-in, a family switch, a
    /// sign-out, a revocation.
    public func modeChanged() async {
        guard launched else { return }
        await background.perform { [weak self] in await self?.evaluate(foreground: false) }
    }

    /// The app became active: re-probe, replay, and allow a foreground
    /// inventory of `Activity.activities`.
    public func sceneBecameActive() async {
        guard launched else { return }
        await background.perform { [weak self] in await self?.evaluate(foreground: true) }
    }

    /// The account coordinator's mode changed, from a synchronous callback:
    /// evaluates in a task this driver keeps track of.
    public func modeDidChange() {
        follow { [weak self] in await self?.modeChanged() }
    }

    /// Evaluations started from synchronous callbacks, until they finish.
    private var followUps: [UUID: Task<Void, Never>] = [:]

    private func follow(_ work: @escaping @MainActor () async -> Void) {
        let id = UUID()
        followUps[id] = Task { [weak self] in
            await work()
            self?.followUps[id] = nil
        }
    }

    /// Returns once every evaluation started from a callback has finished and
    /// the coordinator's pump is idle: a deterministic point for the suites.
    func waitUntilSettled() async {
        while let next = followUps.values.first {
            await next.value
        }
        await coordinator.waitUntilIdle()
    }

    /// ActivityKit said something while no context is installed. Only worth a
    /// new probe when the last one got no answer at all; a definitive "no"
    /// (self-hosted, another instance, an epoch the probe disagrees with) is
    /// not re-asked on every event. The built-in server without the
    /// capability is not a "no": it has a take-down-only context installed,
    /// so its events never arrive here.
    private func eventArrivedWithoutSession() {
        guard launched, probeUnanswered, evaluating == 0 else { return }
        probeUnanswered = false
        follow { [weak self] in
            guard let self else { return }
            await self.background.perform { [weak self] in await self?.evaluate(foreground: false) }
        }
    }

    /// Observes app activation and preference changes on `center`.
    public func observe(_ center: NotificationCenter, becameActive: Notification.Name?, preferencesChanged: Notification.Name?) {
        for observer in observers { observedCenter?.removeObserver(observer) }
        observers = []
        observedCenter = center
        if let becameActive {
            observers.append(center.addObserver(forName: becameActive, object: nil, queue: .main) { [weak self] _ in
                MainActor.assumeIsolated {
                    self?.follow { [weak self] in await self?.sceneBecameActive() }
                }
            })
        }
        if let preferencesChanged {
            observers.append(center.addObserver(forName: preferencesChanged, object: nil, queue: .main) { [weak self] _ in
                MainActor.assumeIsolated { self?.preferencesChanged() }
            })
        }
    }

    /// The feed break limit is part of the registration; anything else in the
    /// defaults is not.
    func preferencesChanged() {
        coordinator.updateFeedBreakMinutes(feedBreakMinutes())
    }

    /// A definitive answer decides: no eligible context means stop, an
    /// eligible one is installed. No answer at all decides nothing: a session
    /// proven earlier for the same realm keeps working, and otherwise the probe
    /// is retried at 2, 5 and 15 seconds, then left to the next trigger.
    private func evaluate(foreground: Bool) async {
        evaluation += 1
        let ticket = evaluation
        evaluating += 1
        defer { evaluating -= 1 }
        var attempt = 0
        while true {
            guard let key = LiveActivityPushEligibility.familyKey(for: mode()), let endpoint = endpoint(), let builtIn,
                  endpoint == builtIn, key.canonicalOrigin == endpoint.origin.canonicalString,
                  key.serverInstanceID == endpoint.serverInstanceID,
                  let environment = environment.environment() else {
                probeUnanswered = false
                await coordinator.stop()
                return
            }
            let info: ServerInfo?
            do { info = try await probe(endpoint.origin) } catch { info = nil }
            // A newer evaluation (a mode change during the probe) owns the
            // outcome; a cancelled one (background time ran out) decides
            // nothing — like a probe with no answer, it leaves the next
            // ActivityKit event to evaluate again.
            guard ticket == evaluation else { return }
            guard !Task.isCancelled else {
                probeUnanswered = true
                return
            }
            guard LiveActivityPushEligibility.familyKey(for: mode()) == key, self.endpoint() == endpoint else {
                // The mode changed under the probe: this answer was for the
                // old realm and says nothing about the new one, which the
                // evaluation for that change decides. Unknown until then.
                probeUnanswered = false
                await coordinator.stop(withdrawingPush: false)
                return
            }
            if let info {
                probeUnanswered = false
                guard let context = LiveActivityPushContext(
                    familyKey: key, endpoint: endpoint, serverInfo: info, builtIn: builtIn,
                    activitiesAllowed: activitiesAllowed(), feedBreakMinutes: feedBreakMinutes(), environment: environment
                ) else {
                    // The probe disagrees with the pinned identity or epoch:
                    // stop sending, but leave the server's registration alone.
                    await coordinator.stop()
                    return
                }
                // Without the capability the context can only take down: a
                // stopped push worker is transient and nothing is torn down
                // for it, but a removal the user already asked for — Live
                // Activities off — still goes to the server, which honours it
                // without push.
                await coordinator.start(context: context)
                guard ticket == evaluation else { return }
                await replay(foreground: foreground)
                return
            }
            // No answer. The context this realm's session was built on — with
            // the capability, or take-down-only without it — came from an
            // earlier probe's answer in this process, and nothing has
            // disproved it: keep that session working through a blip, as it
            // was.
            if let current = coordinator.context, current.familyKey == key, current.endpoint == endpoint,
               current.environment == environment {
                probeUnanswered = false
                coordinator.updateFeedBreakMinutes(feedBreakMinutes())
                await replay(foreground: foreground)
                return
            }
            // Another realm's session must not outlive the change. Nothing is
            // known about the new one yet: not a withdrawal.
            if coordinator.context != nil { await coordinator.stop(withdrawingPush: false) }
            guard attempt < retryDelays.count else {
                probeUnanswered = true
                return
            }
            // A retry wait cut short is the same undecided evaluation.
            do { try await sleep(retryDelays[attempt]) } catch {
                if ticket == evaluation { probeUnanswered = true }
                return
            }
            guard ticket == evaluation else { return }
            guard !Task.isCancelled else {
                probeUnanswered = true
                return
            }
            attempt += 1
        }
    }

    private func replay(foreground: Bool) async {
        if foreground {
            await coordinator.reconcileForeground()
        } else {
            await coordinator.reconcile()
        }
    }
}
