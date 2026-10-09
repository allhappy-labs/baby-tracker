import Foundation
import Observation
import BabyStore

/// Lives in the App Group so a widget tap logs against the baby the app shows.
@MainActor
@Observable
public final class SelectedBabyStore {
    private static let key = "selectedBabyID"
    private let defaults: UserDefaults
    public private(set) var selectedID: UUID?

    public init(defaults: UserDefaults? = nil) {
        self.defaults = defaults
            ?? UserDefaults(suiteName: BabySchema.appGroupID)
            ?? .standard
        selectedID = self.defaults.string(forKey: Self.key).flatMap(UUID.init(uuidString:))
    }

    public func select(_ id: UUID) {
        selectedID = id
        defaults.set(id.uuidString, forKey: Self.key)
    }

    /// A stale selection falls back to the first baby rather than showing an
    /// empty screen — a deleted baby must never strand the app.
    public func resolve(from babies: [Baby]) -> Baby? {
        if let id = selectedID, let match = babies.first(where: { $0.id == id }) {
            return match
        }
        return babies.first
    }
}
