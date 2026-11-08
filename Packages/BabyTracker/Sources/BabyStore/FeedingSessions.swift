import Foundation
import SwiftData
import BabyCore

public struct FeedingSession: Equatable, Sendable {
    public let id: UUID
    public let babyID: UUID
    public let feedIDs: [UUID]
    public let startedAt: Date
    public let endedAt: Date
    public let activeSeconds: Int
    public let measuredVolumeML: Double
}

public struct MeasuredBottleStep: Equatable, Sendable {
    public let feedID: UUID
    public let at: Date
    public let volumeML: Double
}

public enum FeedingSessionError: Error, Equatable {
    case missingFeed
    case differentBabies
    case alreadyOneSession
    case firstFeedCannotSeparate
    /// A session of solids alone never joins and is never joined — a rule
    /// enforced here, not only in the suggestion/history layer above, so no
    /// caller can combine one by going straight to the store. Solids join a
    /// feed only by Add part (`addFeedPart`).
    case solidsNeverJoin
}

public extension BabyStore {
    func measuredBottleSteps(for babyID: UUID, from start: Date, to end: Date) throws -> [MeasuredBottleStep] {
        try sessionFeeds(for: babyID).flatMap { feed -> [MeasuredBottleStep] in
            feed.components.compactMap { component in
                guard let volume = component.volumeML, component.bottleContents != nil else { return nil }
                let at = component.startedAt ?? feed.startedAt
                guard at >= start && at < end else { return nil }
                return MeasuredBottleStep(feedID: feed.id, at: at, volumeML: volume)
            }
        }
    }

    /// Sessions whose first part started in `[start, end)`, as for sleep. A
    /// session belongs to the day its first part started, so a feed joined
    /// across midnight is counted once, on the evening it began — never
    /// again on the day a later part started.
    func feedingSessions(for babyID: UUID, from start: Date, to end: Date) throws -> [FeedingSession] {
        Self.projectSessions(try sessionFeeds(for: babyID))
            .filter { $0.startedAt >= start && $0.startedAt < end }
    }

    /// One local transaction; every changed member gets its ordinary feed
    /// update mutation, and a running part that continued one of the joined
    /// sessions is moved to the combined one.
    func combineFeeds(_ feedIDs: [UUID], for babyID: UUID) throws {
        let expected = try captureWriteContext()
        try withRealmWrite(expected: expected) {
            let feeds = try self.sessionFeeds(for: babyID)
            let selected = feeds.filter { feedIDs.contains($0.id) }
            guard selected.count == Set(feedIDs).count && selected.count >= 2 else { throw FeedingSessionError.missingFeed }
            let groupIDs = Set(selected.map(\.effectiveFeedingSessionID))
            guard groupIDs.count > 1 else { throw FeedingSessionError.alreadyOneSession }
            let affected = feeds.filter { groupIDs.contains($0.effectiveFeedingSessionID) }
            guard groupIDs.allSatisfy({ Self.hasMilk($0, in: affected) }) else {
                throw FeedingSessionError.solidsNeverJoin
            }
            let target = affected.min(by: Self.feedOrder)!.effectiveFeedingSessionID
            for feed in affected where feed.effectiveFeedingSessionID != target {
                feed.feedingSessionID = target
                feed.lastModifiedAt = self.now
                try self.recordLocked(.update, on: feed)
            }
            // A running part continuing a joined-away session follows it, so
            // it stops into the session the parent now sees.
            for group in groupIDs where group != target {
                try self.relinkRunningPartLocked(.feed, for: babyID, from: group, to: target)
            }
        }
    }

    /// Splits a group at a member, keeping the earlier members together; a
    /// running part that continued the group goes with the later half.
    func separateFeed(at feedID: UUID, for babyID: UUID) throws {
        let expected = try captureWriteContext()
        try withRealmWrite(expected: expected) {
            let feeds = try self.sessionFeeds(for: babyID)
            guard let selected = feeds.first(where: { $0.id == feedID }) else { throw FeedingSessionError.missingFeed }
            let session = selected.effectiveFeedingSessionID
            let group = feeds.filter { $0.effectiveFeedingSessionID == session }
                .sorted(by: Self.feedOrder)
            guard let index = group.firstIndex(where: { $0.id == feedID }), index > 0 else {
                throw FeedingSessionError.firstFeedCannotSeparate
            }
            for feed in group[index...] {
                let link: UUID? = feed.id == feedID ? nil : feedID
                if feed.feedingSessionID != link {
                    feed.feedingSessionID = link
                    feed.lastModifiedAt = self.now
                    try self.recordLocked(.update, on: feed)
                }
            }
            // A running part continues after every member, so it goes with
            // the later half.
            try self.relinkRunningPartLocked(.feed, for: babyID, from: session, to: feedID)
        }
    }
}

extension BabyStore {
    func sessionFeeds(for babyID: UUID) throws -> [FeedEntry] {
        let realmKey = activeRealm.encodedKey
        return try context.fetch(FetchDescriptor<FeedEntry>(predicate: #Predicate {
            $0.babyID == babyID && $0.realmKey == realmKey && !$0.isTombstoned
        }))
    }

    static func feedOrder(_ lhs: FeedEntry, _ rhs: FeedEntry) -> Bool {
        lhs.startedAt == rhs.startedAt ? lhs.id.uuidString < rhs.id.uuidString : lhs.startedAt < rhs.startedAt
    }

    static func projectSessions(_ feeds: [FeedEntry]) -> [FeedingSession] {
        Dictionary(grouping: feeds, by: \.effectiveFeedingSessionID).compactMap { id, members in
            let ordered = members.sorted(by: feedOrder)
            guard let first = ordered.first else { return nil }
            return FeedingSession(
                id: id, babyID: first.babyID, feedIDs: ordered.map(\.id), startedAt: first.startedAt,
                endedAt: ordered.map { $0.endedAt ?? $0.startedAt }.max() ?? first.startedAt,
                activeSeconds: ordered.reduce(0) { total, feed in
                    total + feed.components.reduce(0) { $0 + ($1.durationSeconds ?? $1.bottleSeconds ?? 0) }
                },
                measuredVolumeML: ordered.reduce(0) { $0 + $1.totalBottleVolumeML }
            )
        }.sorted { $0.startedAt == $1.startedAt ? $0.id.uuidString < $1.id.uuidString : $0.startedAt < $1.startedAt }
    }

}
