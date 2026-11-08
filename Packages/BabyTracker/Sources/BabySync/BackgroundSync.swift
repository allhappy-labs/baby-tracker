import BabyStore
import BabyCore
import Foundation

/// Bounded catch-up for the places that are not the foreground app.
///
/// A widget, an App Intent and a background refresh all get exactly one sync
/// attempt and no loop. They are short-lived and budgeted; a retry loop in an
/// extension spends the user's battery on work the app will redo properly the
/// moment it is opened.
@MainActor
public final class BackgroundSync {
    /// The production background-refresh identifier. One, declared here so it
    /// cannot drift from the Info.plist entry that permits it.
    public static let refreshTaskIdentifier = AppIdentity.identifier("sync.refresh")

    public private(set) var attempts = 0

    private let coordinator: SyncCoordinator
    private let store: BabyStore
    private let accountKey: AccountSessionKey

    public init(coordinator: SyncCoordinator, store: BabyStore, accountKey: AccountSessionKey) {
        self.coordinator = coordinator
        self.store = store
        self.accountKey = accountKey
    }

    /// One attempt, or none at all if this account may not sync.
    ///
    /// The gate is rechecked here rather than trusted from the app: an
    /// extension runs in its own process and may never have seen the sign-out
    /// that happened while it was not running.
    @discardableResult
    public func runOnce(reason: SyncReason) async -> Bool {
        guard (try? store.accountAllowsOrdinarySync(accountKey.leaseKey)) == true else { return false }
        guard case .shared = store.activeRealm else { return false }
        attempts += 1
        await coordinator.syncOnce(reason: reason)
        return true
    }
}

/// When the foreground app should sync.
///
/// Launch and foreground are catch-up; a local write is an immediate push so
/// the other caregiver sees it now rather than at the next poll. Polling is
/// the fallback for a server with no live hints, and is deliberately slow —
/// a family tracker is not a chat app, and a one-minute poll on cellular is a
/// battery complaint waiting to happen.
public enum SyncScheduler {
    public static let pollingInterval: TimeInterval = 300

    public static func shouldSyncImmediately(for reason: SyncReason) -> Bool {
        switch reason {
        case .launch, .foreground, .localWrite, .liveHint, .backgroundPush, .extensionRequest: true
        }
    }

    /// Whether the short foreground poll is needed. With live hints the socket
    /// is the signal, and only a safety poll at `pollingInterval` stays while
    /// it is up: hints can be lost, and the poll task is also what tells the
    /// engine the app is in the foreground.
    public static func needsPolling(serverInfo: ServerInfo) -> Bool {
        !serverInfo.advertisesLiveHints
    }
}
