import Foundation
import BabyCore
import Network

/// Whether the phone has any usable network path, as the system sees it.
///
/// A VPN tunnel that says "Connected" and carries nothing still leaves the
/// path satisfied; airplane mode or no signal does not. That difference is
/// what lets sync tell "this phone is offline" from "the server is out of
/// reach" (`FamilySyncEngine.Status.serverUnreachable`).
public final class NetworkPath: @unchecked Sendable {
    public static let shared = NetworkPath()

    private let monitor = NWPathMonitor()
    private let lock = NSLock()
    /// Until the first update arrives, assume a network: the cost is one
    /// "server out of reach" a few minutes early, never a missed one.
    private var satisfied = true

    private init() {
        monitor.pathUpdateHandler = { [weak self] path in
            guard let self else { return }
            lock.withLock { satisfied = path.status == .satisfied }
        }
        monitor.start(queue: DispatchQueue(label: AppIdentity.identifier("network-path")))
    }

    public var isSatisfied: Bool { lock.withLock { satisfied } }
}
