import Foundation
import BabyCore
import BabyStore

/// A Lock Screen or Dynamic Island button on a card with a realm.
public enum LiveActivityCardAction: Equatable, Sendable {
    case stopFeed
    case startBreast(BreastSide)
    case startBottle
    case wake

    var kind: TimerKind {
        switch self {
        case .stopFeed, .startBreast, .startBottle: .feed
        case .wake: .sleep
        }
    }
}

/// What a card action did.
public enum LiveActivityActionOutcome: Equatable, Sendable {
    /// Written, and handed to one bounded sync in the background to publish.
    case performed
    /// What the card showed had ended or been replaced: the card was brought
    /// in line, nothing was written.
    case alreadyFinished
    /// This phone may not write in the family: nothing was written.
    case readOnly
    /// The card belonged to another realm: it was taken down, nothing written.
    case wrongRealm
    /// Nothing was written; the reason was parked for the app.
    case handedOff(LiveActivityHandoffReason)
}

/// The buttons of a card that belongs to a family realm.
///
/// Each one is checked against the family's current state before it writes
/// (`LiveActivityActionGate`, in production the background composition the
/// App Intent builds): an obsolete card never stops, wakes or replaces the
/// timer that runs now. A card without a realm never comes here — its
/// buttons keep `OutOfAppFeedActions` and today's path.
///
/// What an authorized action writes is exactly what the app's own controls
/// write: the same store calls, through `OutOfAppFeedActions` for a feed — a
/// timed bottle is saved at the previous bottle's amount with one "Set amount" request
/// for later — and the card shows it at once as this phone's own change.
@MainActor
public enum LiveActivityCardActions {
    /// How long the catch-up before an action may take — needed only when
    /// the card shows family state this phone has not applied. A tap on a
    /// locked phone cannot wait longer.
    public static let timeout: Duration = .seconds(5)

    /// How long the publish after a write may take. The tap does not wait on
    /// it: it runs through `runInBackground`, and offline the mutation stays
    /// in the durable outbox for the app.
    public static let publishTimeout: Duration = .seconds(20)

    /// Runs the publish after the intent has returned. The app installs one
    /// that holds a background-task assertion and cancels the work when the
    /// time runs out, so nothing keeps running into a suspension.
    public static var runInBackground: @MainActor (@escaping @MainActor () async -> Void) -> Void = { work in
        Task { @MainActor in await work() }
    }

    /// Builds the gate for one action. Installed by the app at launch: Live
    /// Activity intents run in the app's process. Nil in any process that
    /// cannot reach the family server, where a card with a realm hands off.
    public static var makeGate: (@MainActor (BabyStore) -> (any LiveActivityActionGate)?)?

    /// Checks `target` and acts if it may. The gate is finished before this
    /// returns, except after a write: then it is handed, with the publish, to
    /// `background`, which finishes it once the publish is done — the tap
    /// itself never waits on the network to send what it wrote.
    public static func perform(
        _ action: LiveActivityCardAction,
        target: LiveActivityActionTarget,
        babyID: UUID,
        in store: BabyStore,
        gate: any LiveActivityActionGate,
        pending: PendingQuickLogStore = PendingQuickLogStore(),
        timeout: Duration = LiveActivityCardActions.timeout,
        background: @MainActor (@escaping @MainActor () async -> Void) -> Void = LiveActivityCardActions.runInBackground
    ) async throws -> LiveActivityActionOutcome {
        var publishes = false
        defer { if !publishes { gate.finish() } }
        guard target.scope.babyID == babyID, target.scope.kindRaw == action.kind.rawValue else {
            return handOff(.unverifiable, pending: pending)
        }
        switch await gate.prepare(target: target, timeout: timeout) {
        case .openApp(let reason):
            return handOff(reason, pending: pending)
        case .wrongRealm:
            await reconcileCard(action.kind, babyID: babyID, in: store)
            return .wrongRealm
        case .readOnly:
            await reconcileCard(action.kind, babyID: babyID, in: store)
            return .readOnly
        case .alreadyFinished:
            await reconcileCard(action.kind, babyID: babyID, in: store)
            return .alreadyFinished
        case .ready(let context):
            let wrote: Bool
            do {
                wrote = try await write(action, target: target, babyID: babyID, in: store, context: context, pending: pending)
            } catch WritePermissionError.readOnlyCaregiver {
                // Demoted between the check and the write: the boundary
                // refused it and rolled back.
                await reconcileCard(action.kind, babyID: babyID, in: store)
                return .readOnly
            } catch is RealmWriteError {
                // The realm moved between the check and the write.
                return handOff(.sessionChanged, pending: pending)
            }
            guard wrote else {
                await reconcileCard(action.kind, babyID: babyID, in: store)
                return .alreadyFinished
            }
            publishes = true
            background {
                await gate.publish(timeout: LiveActivityCardActions.publishTimeout)
                gate.finish()
            }
            return .performed
        }
    }

    /// The write itself, after the lifecycle is read once more: no await
    /// separates this reading from the write, so it is the one written.
    /// False when there is nothing the card's button means to change.
    private static func write(
        _ action: LiveActivityCardAction,
        target: LiveActivityActionTarget,
        babyID: UUID,
        in store: BabyStore,
        context: RealmWriteContext,
        pending: PendingQuickLogStore
    ) async throws -> Bool {
        if target.isPushed {
            guard target.names(try LiveActivityController.lifecycle(kind: action.kind, babyID: babyID, in: store)) else { return false }
        }
        switch action {
        case .stopFeed:
            guard let timer = try store.activeTimer(kind: .feed, for: babyID) else { return false }
            try await OutOfAppFeedActions.stopFeed(timer, in: store, pending: pending, expectedWriteContext: context)
        case .startBreast(let side):
            try await OutOfAppFeedActions.startBreast(side, for: babyID, in: store, pending: pending, expectedWriteContext: context)
        case .startBottle:
            try await OutOfAppFeedActions.startBottle(for: babyID, in: store, pending: pending, expectedWriteContext: context)
        case .wake:
            guard let sleep = try store.activeTimer(kind: .sleep, for: babyID) else { return false }
            try await LiveActivityController.stopping(sleep, in: store) {
                _ = try store.stopSleep(sleep, expectedWriteContext: context)
            }
        }
        return true
    }

    /// Brings the card in line with what the store has, as a catch-up rather
    /// than this phone's own change: a card with newer server content is
    /// still protected by the ordering rule, and a card of another realm goes.
    private static func reconcileCard(_ kind: TimerKind, babyID: UUID, in store: BabyStore) async {
        switch kind {
        case .sleep:
            await LiveActivityController.showSleepAsStored(running: try? store.activeTimer(kind: .sleep, for: babyID),
                                                           babyID: babyID, in: store)
        case .feed:
            await LiveActivityController.enterBreak(kind: .feed, babyID: babyID, in: store)
        }
    }

    private static func handOff(_ reason: LiveActivityHandoffReason, pending: PendingQuickLogStore) -> LiveActivityActionOutcome {
        pending.parkActivityHandoff(reason)
        return .handedOff(reason)
    }
}
