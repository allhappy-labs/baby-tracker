import Foundation

/// App-layer inputs expressed without a dependency on the family settings module.
public enum HomeSyncPhase: Equatable {
    case idle, syncing, current(Date), renewing, offline, serverUnreachable, needsSignIn, failed(String)
}

/// What Home shows about sync. Only states a parent can act on or should know
/// about get a card; routine syncing, a fresh pull and entries on their way
/// stay quiet, and Settings carries the full status.
public enum HomeSyncStatus: Equatable {
    case hidden, offline(Int), serverUnreachable(Int), needsSignIn(Int), recoveryFailed(String)

    public static func resolve(_ phase: HomeSyncPhase, pendingDeliveryCount: Int) -> Self {
        let pending = max(0, pendingDeliveryCount)
        return switch phase {
        case .needsSignIn: .needsSignIn(pending)
        case .offline: .offline(pending)
        case .serverUnreachable: .serverUnreachable(pending)
        case .failed: .recoveryFailed("retry")
        case .idle, .syncing, .current, .renewing: .hidden
        }
    }

    public var offersSignIn: Bool {
        if case .needsSignIn = self { return true }
        return false
    }

    public var showsQueuedExplanation: Bool {
        switch self {
        case .needsSignIn(let count), .offline(let count), .serverUnreachable(let count): count > 0
        default: false
        }
    }
}
