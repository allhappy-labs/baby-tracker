import Foundation
import SwiftData
import BabyCore

/// Pre-fills that make bottle logging two taps: what the previous bottle was.
/// Reads history rather than asking the user to configure anything.
@MainActor
public struct LogDefaults {
    public static let fallbackVolumeML: Double = 120
    public static let fallbackContents: BottleContents = .formula

    private let store: BabyStore

    public init(store: BabyStore) {
        self.store = store
    }

    /// The baby's most recent bottle feed.
    ///
    /// Fetched ascending and read from the end rather than fetched descending
    /// and read from the front: SwiftData's sort is stable, and ties (e.g. two
    /// bottles logged in the same test tick) keep their original insertion
    /// order regardless of the requested sort direction. Reading the tail of
    /// an ascending fetch means the most-recently-inserted entry wins even
    /// when timestamps tie; reading the head of a descending fetch does not.
    private func lastBottle(for babyID: UUID) throws -> FeedEntry? {
        let bottle = FeedKind.bottle.rawValue
        let realmKey = store.activeRealm.encodedKey
        var descriptor = FetchDescriptor<FeedEntry>(
            predicate: #Predicate { $0.babyID == babyID && $0.realmKey == realmKey && !$0.isTombstoned && $0.kindRaw == bottle }
        )
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .forward)]
        return try store.context.fetch(descriptor).last
    }

    /// What the previous bottle held. Never a bottle saved at 0 ml: every
    /// total leaves a zero-volume bottle out, so it is not an amount to
    /// repeat.
    public func lastBottleVolumeML(for babyID: UUID) throws -> Double {
        guard let volume = try lastBottle(for: babyID)?.bottleComponents.last?.volumeML, volume > 0 else {
            return Self.fallbackVolumeML
        }
        return volume
    }

    public func lastBottleContents(for babyID: UUID) throws -> BottleContents {
        try lastBottle(for: babyID)?.bottleComponents.last?.bottleContents ?? Self.fallbackContents
    }
}
