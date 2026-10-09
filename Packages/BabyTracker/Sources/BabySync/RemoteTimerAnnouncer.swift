import BabyStore
import Foundation

/// What happens on screen when another caregiver's timer starts, changes or
/// ends on this phone. A seam so the engine's decisions — what to announce,
/// and when — are testable on macOS, where every ActivityKit call is a no-op.
@MainActor
public protocol RemoteTimerAnnouncer {
    func started(_ timer: ActiveTimer, in store: BabyStore) async
    func changed(_ timer: ActiveTimer, in store: BabyStore) async
    func ended(timerID: UUID, liveActivityID: String?, in store: BabyStore) async
    /// A sync succeeded: the store now holds acknowledged state up to its
    /// cursor, so cards the server pushed to may be brought in line with it.
    func syncSucceeded(in store: BabyStore) async
}

public extension RemoteTimerAnnouncer {
    func syncSucceeded(in store: BabyStore) async {}
}

/// The production announcer: the Lock Screen card, through the one type that
/// owns ActivityKit.
public struct LiveActivityRemoteTimerAnnouncer: RemoteTimerAnnouncer {
    public init() {}

    /// Another caregiver's part starts on the card already up for that baby
    /// and kind — a break card goes back to running — or on a new one. Where
    /// the server pushes to the cards, the new one is its push-to-start.
    public func started(_ timer: ActiveTimer, in store: BabyStore) async {
        await LiveActivityController.adoptRemoteTimer(timer, in: store)
    }

    /// Another caregiver changed a running timer. Not this phone's own
    /// change: a snapshot that may already be older than what the server
    /// pushed, so it goes through the ordering rule rather than straight onto
    /// the card.
    public func changed(_ timer: ActiveTimer, in store: BabyStore) async {
        await LiveActivityController.update(for: timer, in: store, own: false)
    }

    /// Another caregiver's stop moves this phone's card into the break that
    /// follows, as their own card does — or ends it when no break follows.
    /// The card is found by its id, which is the only thing left of the
    /// timer; its baby and kind are on the card.
    public func ended(timerID: UUID, liveActivityID: String?, in store: BabyStore) async {
        await LiveActivityController.enterBreak(activityID: liveActivityID, in: store)
    }

    /// Where the server pushes to the cards, the reconciliation the
    /// foreground pass deferred: it runs only on state the server has
    /// acknowledged. Elsewhere nothing.
    public func syncSucceeded(in store: BabyStore) async {
        await LiveActivityController.reconcileAfterSync(in: store)
    }
}
