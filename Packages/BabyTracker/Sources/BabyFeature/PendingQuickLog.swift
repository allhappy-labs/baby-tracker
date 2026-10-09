import Foundation
import BabyStore

/// Something a tap outside the app left for the app to finish, parked until
/// the app is next on screen.
///
/// Mostly that is one thing: the amount of a timed bottle ended outside the
/// app — Stop on the Lock Screen, or L/R stopping it there. That tap never
/// opens the app (it must work on a locked phone), so the feed is saved at
/// the usual amount there and then, and this asks the app to offer "Set
/// amount" for that feed the next time it is on screen.
///
/// The other is a Lock Screen action handed to the app because the card
/// could not be checked against the family's current state: its reason,
/// kept apart from the bottle's request (`parkActivityHandoff`).
///
/// Lives in the App Group because the request may be written by an extension
/// process; the notification covers the case where the app is already on
/// screen and will not change scene phase.
@MainActor
public struct PendingQuickLogRequest: Equatable {
    public enum Action: Equatable, Sendable {
        /// Offer to correct the amount of a bottle that was saved without
        /// one being asked for.
        case bottleAmount(feedID: UUID)
    }

    public let action: Action
    public let babyID: UUID?

    public init(_ action: Action, babyID: UUID? = nil) {
        self.action = action
        self.babyID = babyID
    }
}

/// The one thing the app acts on at an activation
/// (`PendingQuickLogStore.consumeNextActivation`).
@MainActor
public enum PendingActivation: Equatable {
    /// A Lock Screen action was handed to the app: say why, once.
    case activityHandoff(LiveActivityHandoffReason)
    /// Something a tap outside the app left to finish.
    case quickLog(PendingQuickLogRequest)
}

@MainActor
public final class PendingQuickLogStore {
    /// Posted after a request is parked, so an app that is already on screen
    /// acts on it immediately rather than at its next activation.
    public static let didRequest = Notification.Name("com.olhapi.babytracker.pendingQuickLog")

    private static let babyKey = "pendingQuickLogBabyID"
    private static let bottleAmountKey = "pendingQuickLogBottleAmountFeedID"
    /// Kept apart from the bottle's keys: a handoff and a "Set amount" are
    /// independent, and parking one never clears the other.
    private static let activityHandoffKey = "pendingQuickLogActivityHandoff"
    /// Written by earlier builds — a widget's "Feed" tap and a Lock Screen
    /// bottle Stop that opened the app. Nothing writes them now; they are only
    /// cleared, so an update never replays a stale tap.
    private static let legacyKeys = ["pendingQuickLog", "pendingQuickLogFinishBottleAndStop"]
    private let defaults: UserDefaults
    private let center: NotificationCenter

    public init(defaults: UserDefaults? = nil, center: NotificationCenter = .default) {
        self.defaults = defaults
            ?? UserDefaults(suiteName: BabySchema.appGroupID)
            ?? .standard
        self.center = center
    }

    /// Parks `action`, replacing whatever was parked before.
    public func request(_ action: PendingQuickLogRequest.Action, babyID: UUID? = nil) {
        clear()
        if let babyID {
            defaults.set(babyID.uuidString, forKey: Self.babyKey)
        }
        switch action {
        case .bottleAmount(let feedID):
            defaults.set(feedID.uuidString, forKey: Self.bottleAmountKey)
        }
        center.post(name: Self.didRequest, object: nil)
    }

    /// Reads the request and clears it in one step: a parked tap is acted on
    /// exactly once, never replayed on the next launch.
    public func consume() -> PendingQuickLogRequest? {
        let feedID = defaults.string(forKey: Self.bottleAmountKey).flatMap(UUID.init(uuidString:))
        let babyID = defaults.string(forKey: Self.babyKey).flatMap(UUID.init(uuidString:))
        clear()
        return feedID.map { PendingQuickLogRequest(.bottleAmount(feedID: $0), babyID: babyID) }
    }

    /// A Lock Screen action that could not be verified against the family's
    /// current state: the app, once open, says why. Only the reason is
    /// parked — never the action, which is never replayed.
    public func parkActivityHandoff(_ reason: LiveActivityHandoffReason) {
        defaults.set(reason.rawValue, forKey: Self.activityHandoffKey)
        center.post(name: Self.didRequest, object: nil)
    }

    /// Reads the parked handoff and clears it: shown once.
    public func consumeActivityHandoff() -> LiveActivityHandoffReason? {
        let reason = defaults.string(forKey: Self.activityHandoffKey).flatMap(LiveActivityHandoffReason.init(rawValue:))
        defaults.removeObject(forKey: Self.activityHandoffKey)
        return reason
    }

    /// What the app shows on one activation, one thing at a time: the
    /// handoff's explanation first, then the bottle's "Set amount". Only the
    /// one returned is consumed; the other waits for the next activation —
    /// an alert and a sheet presented together would fight over the screen.
    public func consumeNextActivation() -> PendingActivation? {
        if let reason = consumeActivityHandoff() { return .activityHandoff(reason) }
        return consume().map(PendingActivation.quickLog)
    }

    /// `consumeNextActivation` for a screen that is already explaining a
    /// handoff: nothing is consumed while that alert is up. A bottle Stop on
    /// the Lock Screen while the alert shows posts `didRequest` into the
    /// running app; consuming then would present "Set amount" over the alert —
    /// the two would fight for the screen — and the request would be gone.
    /// Left parked, it is read when the alert is dismissed.
    public func consumeNextActivation(whileShowingHandoff showing: Bool) -> PendingActivation? {
        guard !showing else { return nil }
        return consumeNextActivation()
    }

    private func clear() {
        for key in [Self.babyKey, Self.bottleAmountKey] + Self.legacyKeys {
            defaults.removeObject(forKey: key)
        }
    }
}
