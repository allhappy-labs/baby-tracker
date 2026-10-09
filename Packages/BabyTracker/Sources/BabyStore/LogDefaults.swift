import Foundation
import SwiftData
import BabyCore

/// Pre-fills that make bottle logging two taps. Reads history rather than
/// asking the user to configure anything.
@MainActor
public struct LogDefaults {
    public static let fallbackVolumeML: Double = 120
    public static let fallbackContents: BottleContents = .formula

    private let store: BabyStore

    public init(store: BabyStore) {
        self.store = store
    }

    private func recentBottles(for babyID: UUID, limit: Int = 30) throws -> [FeedEntry] {
        let bottle = FeedKind.bottle.rawValue
        let realmKey = store.activeRealm.encodedKey
        var descriptor = FetchDescriptor<FeedEntry>(
            predicate: #Predicate { $0.babyID == babyID && $0.realmKey == realmKey && !$0.isTombstoned && $0.kindRaw == bottle }
        )
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .reverse)]
        descriptor.fetchLimit = limit
        return try store.context.fetch(descriptor)
    }

    /// Volume is habitual, so the mode beats the most recent value.
    public func mostUsedBottleVolumeML(for babyID: UUID) throws -> Double {
        let volumes = try recentBottles(for: babyID).compactMap { $0.bottleComponents.last?.volumeML }
        guard !volumes.isEmpty else { return Self.fallbackVolumeML }

        let counts = volumes.reduce(into: [Double: Int]()) { $0[$1, default: 0] += 1 }
        // Most frequent wins; ties break toward the larger volume for determinism.
        return counts.sorted { ($0.value, $0.key) > ($1.value, $1.key) }
            .first?.key ?? Self.fallbackVolumeML
    }

    /// Contents changes more often than volume, so the most recent wins.
    ///
    /// Fetched ascending and read from the end rather than fetched descending
    /// and read from the front: SwiftData's sort is stable, and ties (e.g. two
    /// bottles logged in the same test tick) keep their original insertion
    /// order regardless of the requested sort direction. Reading the tail of
    /// an ascending fetch means the most-recently-inserted entry wins even
    /// when timestamps tie; reading the head of a descending fetch does not.
    public func lastBottleContents(for babyID: UUID) throws -> BottleContents {
        let bottle = FeedKind.bottle.rawValue
        let realmKey = store.activeRealm.encodedKey
        var descriptor = FetchDescriptor<FeedEntry>(
            predicate: #Predicate { $0.babyID == babyID && $0.realmKey == realmKey && !$0.isTombstoned && $0.kindRaw == bottle }
        )
        descriptor.sortBy = [SortDescriptor(\.startedAt, order: .forward)]
        return try store.context.fetch(descriptor).last?.bottleComponents.last?.bottleContents ?? Self.fallbackContents
    }
}
