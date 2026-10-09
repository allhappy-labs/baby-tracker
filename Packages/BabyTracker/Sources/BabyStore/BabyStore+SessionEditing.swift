import Foundation
import SwiftData
import BabyCore

/// A completed part added to an existing session from History.
public enum CompletedFeedPart: Equatable, Sendable {
    case breast(side: BreastSide, from: Date, to: Date)
    case bottle(volumeML: Double, contents: BottleContents, at: Date)
}

/// The session a Join row offers to join: the newest earlier one of the
/// same kind.
public struct PreviousSession: Equatable, Sendable {
    /// The first member's id.
    public let rootID: UUID
    public let lastMemberID: UUID
    public let startedAt: Date
    public let endedAt: Date

    public init(rootID: UUID, lastMemberID: UUID, startedAt: Date, endedAt: Date) {
        self.rootID = rootID
        self.lastMemberID = lastMemberID
        self.startedAt = startedAt
        self.endedAt = endedAt
    }
}

/// Wherever these calls take a session's `rootID`, it may name the
/// session's own id (`FeedingSession.id`, `SleepSession.id`) — which, when
/// the other phone tombstoned the first part, can be a deleted entry's — or
/// any live member of the session. The session's own id wins: after a Join,
/// parts that continued the joined-away session can still be keyed by its
/// first part, now a member of the earlier session, and a caller holding
/// that key means those parts (final review I2). A caller holding an entry
/// id resolves the entry's session first and passes its id.
public extension BabyStore {
    /// Logs `part` and links it to the session whose root is `rootID`,
    /// whatever the break limit says (manual add).
    ///
    /// One write and one `create`, already carrying the link. `logBreast` /
    /// `logBottle` can't be wrapped for this: the write boundary is not
    /// reentrant, and logging first then relinking would send the other
    /// phone a create that joined whatever the break limit chose, followed
    /// by an update undoing it. They share the entry builders instead.
    @discardableResult
    func addFeedPart(_ part: CompletedFeedPart, toSession rootID: UUID, for babyID: UUID) throws -> FeedEntry {
        let build: () -> FeedEntry
        switch part {
        case let .breast(side, start, end):
            let seconds = try Self.breastSeconds(from: start, to: end)
            build = { self.breastEntry(side: side, seconds: seconds, from: start, to: end, for: babyID) }
        case let .bottle(volumeML, contents, at):
            build = { self.bottleEntry(volumeML: volumeML, contents: contents, at: at, for: babyID) }
        }
        let expected = try captureWriteContext()
        return try withRealmWrite(expected: expected) {
            let feeds = try self.sessionFeeds(for: babyID)
            let session = try Self.feedSessionKey(rootID, in: feeds)
            guard !feeds.contains(where: { $0.effectiveFeedingSessionID == session && $0.kind == .solids }) else {
                throw FeedingSessionError.solidsNeverJoin
            }
            let entry = build()
            entry.feedingSessionID = session
            self.context.insert(entry)
            try self.recordLocked(.create, on: entry)
            return entry
        }
    }

    /// `addFeedPart` for a sleep: one write, one `create` carrying the link.
    @discardableResult
    func addSleepPart(from start: Date, to end: Date, toSession rootID: UUID, for babyID: UUID) throws -> SleepEntry {
        let expected = try captureWriteContext()
        return try withRealmWrite(expected: expected) {
            let session = try Self.sleepSessionKey(rootID, in: try self.sessionSleeps(for: babyID))
            let entry = self.sleepEntry(from: start, to: end, for: babyID)
            entry.sleepSessionID = session
            self.context.insert(entry)
            try self.recordLocked(.create, on: entry)
            return entry
        }
    }

    /// Tombstones every member of the session in one write. A running part
    /// that had joined the session is detached, so it ends as a session of
    /// its own rather than one linked to deleted entries.
    func deleteSession(_ kind: SessionKind, rootID: UUID, for babyID: UUID) throws {
        let expected = try captureWriteContext()
        try withRealmWrite(expected: expected) {
            let session: UUID
            switch kind {
            case .feed:
                let feeds = try self.sessionFeeds(for: babyID)
                session = try Self.feedSessionKey(rootID, in: feeds)
                for feed in feeds where feed.effectiveFeedingSessionID == session { try self.tombstoneLocked(feed) }
            case .sleep:
                let sleeps = try self.sessionSleeps(for: babyID)
                session = try Self.sleepSessionKey(rootID, in: sleeps)
                for sleep in sleeps where sleep.effectiveSleepSessionID == session { try self.tombstoneLocked(sleep) }
            }
            try self.relinkRunningPartLocked(kind, for: babyID, from: session, to: nil)
        }
    }

    /// Deletes one feed entry from its session. Deleting the root promotes
    /// the next member, so the session stays keyed by a live entry; use this,
    /// not `softDelete`, for any feed that may be in a session.
    func deleteFeedPart(_ feedID: UUID, for babyID: UUID) throws {
        let expected = try captureWriteContext()
        try withRealmWrite(expected: expected) {
            let feeds = try self.sessionFeeds(for: babyID)
            guard let feed = feeds.first(where: { $0.id == feedID }) else { throw FeedingSessionError.missingFeed }
            try self.deleteFeedPartLocked(feed, among: feeds)
        }
    }

    /// `deleteFeedPart` for a sleep.
    func deleteSleepPart(_ sleepID: UUID, for babyID: UUID) throws {
        let expected = try captureWriteContext()
        try withRealmWrite(expected: expected) {
            let sleeps = try self.sessionSleeps(for: babyID)
            guard let sleep = sleeps.first(where: { $0.id == sleepID }) else { throw SleepSessionError.missingSleep }
            try self.deleteSleepPartLocked(sleep, among: sleeps)
        }
    }

    /// Removes one component from a feed entry; removes the entry if it was the last.
    func removeComponent(_ componentID: UUID, fromFeed feedID: UUID, for babyID: UUID) throws {
        let expected = try captureWriteContext()
        try withRealmWrite(expected: expected) {
            let feeds = try self.sessionFeeds(for: babyID)
            guard let feed = feeds.first(where: { $0.id == feedID }) else { throw PartError.missingFeed }
            guard let index = feed.components.firstIndex(where: { $0.id == componentID }) else {
                throw PartError.missingComponent
            }
            guard feed.components.count > 1 else { return try self.deleteFeedPartLocked(feed, among: feeds) }
            feed.components.remove(at: index)
            Self.fitToComponents(feed)
            feed.lastModifiedAt = self.now
            try self.recordLocked(.update, on: feed)
        }
    }

    /// The newest earlier session of the same kind (feeds: never a solids-only session).
    /// Nil too when `rootID`'s own session is solids, which never joins.
    func previousSession(_ kind: SessionKind, before rootID: UUID, for babyID: UUID) throws -> PreviousSession? {
        switch kind {
        case .feed:
            let feeds = try sessionFeeds(for: babyID)
            let session = try Self.feedSessionKey(rootID, in: feeds)
            guard !feeds.contains(where: { $0.effectiveFeedingSessionID == session && $0.kind == .solids }) else {
                return nil
            }
            let sessions = Self.projectSessions(feeds.filter { $0.kind != .solids })
            guard let index = sessions.firstIndex(where: { $0.id == session }), index > 0 else { return nil }
            let previous = sessions[index - 1]
            guard let first = previous.feedIDs.first, let last = previous.feedIDs.last else { return nil }
            return PreviousSession(rootID: first, lastMemberID: last, startedAt: previous.startedAt, endedAt: previous.endedAt)
        case .sleep:
            let sleeps = try sessionSleeps(for: babyID)
            let session = try Self.sleepSessionKey(rootID, in: sleeps)
            let sessions = Self.projectSleepSessions(sleeps)
            guard let index = sessions.firstIndex(where: { $0.id == session }), index > 0 else { return nil }
            let previous = sessions[index - 1]
            guard let first = previous.sleepIDs.first, let last = previous.sleepIDs.last else { return nil }
            // An open earlier sleep has no session end; its latest known
            // moment stands in for it.
            let members = sleeps.filter { previous.sleepIDs.contains($0.id) }
            let end = previous.endedAt ?? members.map { $0.endedAt ?? $0.startedAt }.max() ?? previous.startedAt
            return PreviousSession(rootID: first, lastMemberID: last, startedAt: previous.startedAt, endedAt: end)
        }
    }

    /// The end of the part just before `feedID` in its session, if any — the
    /// latest end among the members ordered before it, as a running timer's
    /// check uses the whole session's end (`previousPartEnd(of:)`).
    func previousPartEnd(ofFeed feedID: UUID, for babyID: UUID) throws -> Date? {
        let feeds = try sessionFeeds(for: babyID)
        guard let feed = feeds.first(where: { $0.id == feedID }) else { throw PartError.missingFeed }
        let group = feeds.filter { $0.effectiveFeedingSessionID == feed.effectiveFeedingSessionID }
            .sorted(by: Self.feedOrder)
        guard let index = group.firstIndex(where: { $0.id == feedID }) else { return nil }
        return group[..<index].map { $0.endedAt ?? $0.startedAt }.max()
    }

    /// The start of the part just after `feedID` in its session, if any —
    /// the earliest start among the members ordered after it.
    func nextPartStart(ofFeed feedID: UUID, for babyID: UUID) throws -> Date? {
        let feeds = try sessionFeeds(for: babyID)
        guard let feed = feeds.first(where: { $0.id == feedID }) else { throw PartError.missingFeed }
        let group = feeds.filter { $0.effectiveFeedingSessionID == feed.effectiveFeedingSessionID }
            .sorted(by: Self.feedOrder)
        guard let index = group.firstIndex(where: { $0.id == feedID }) else { return nil }
        return group[(index + 1)...].map(\.startedAt).min()
    }

    /// Where an edited part may start within its session: not before the
    /// previous part ended (ruling H-R4) — `PartError.startBeforePreviousPart`
    /// — and not after the next part began (ruling H-R15), which would
    /// reorder the session behind its root — `PartError.startAfterNextPart`.
    func checkPartStart(_ start: Date, ofFeed feedID: UUID, for babyID: UUID) throws {
        if let previousEnd = try previousPartEnd(ofFeed: feedID, for: babyID), start < previousEnd {
            throw PartError.startBeforePreviousPart
        }
        if let nextStart = try nextPartStart(ofFeed: feedID, for: babyID), start > nextStart {
            throw PartError.startAfterNextPart
        }
    }

    /// `previousPartEnd(ofFeed:)` for a sleep: the latest end among the
    /// sleeps ordered before it in its session. An open sleep counts by its
    /// start, its latest known moment.
    func previousPartEnd(ofSleep sleepID: UUID, for babyID: UUID) throws -> Date? {
        let (group, index) = try sleepSessionPosition(sleepID, for: babyID)
        return group[..<index].map { $0.endedAt ?? $0.startedAt }.max()
    }

    /// `nextPartStart(ofFeed:)` for a sleep.
    func nextPartStart(ofSleep sleepID: UUID, for babyID: UUID) throws -> Date? {
        let (group, index) = try sleepSessionPosition(sleepID, for: babyID)
        return group[(index + 1)...].map(\.startedAt).min()
    }

    /// `checkPartStart` for a sleep (rulings H-R4, H-R15): the same two
    /// bounds, the same two errors.
    func checkSleepPartStart(_ start: Date, ofSleep sleepID: UUID, for babyID: UUID) throws {
        if let previousEnd = try previousPartEnd(ofSleep: sleepID, for: babyID), start < previousEnd {
            throw PartError.startBeforePreviousPart
        }
        if let nextStart = try nextPartStart(ofSleep: sleepID, for: babyID), start > nextStart {
            throw PartError.startAfterNextPart
        }
    }
}

extension BabyStore {
    /// `sleepID`'s session in order, and its place in it.
    func sleepSessionPosition(_ sleepID: UUID, for babyID: UUID) throws -> (group: [SleepEntry], index: Int) {
        let sleeps = try sessionSleeps(for: babyID)
        guard let sleep = sleeps.first(where: { $0.id == sleepID }) else { throw SleepSessionError.missingSleep }
        let group = sleeps.filter { $0.effectiveSleepSessionID == sleep.effectiveSleepSessionID }
            .sorted(by: Self.sleepOrder)
        guard let index = group.firstIndex(where: { $0.id == sleepID }) else { throw SleepSessionError.missingSleep }
        return (group, index)
    }

    /// The grouping key of the session `rootID` names (see the note above):
    /// a session keyed by it first, then the session of a member with it.
    static func feedSessionKey(_ rootID: UUID, in feeds: [FeedEntry]) throws -> UUID {
        if feeds.contains(where: { $0.effectiveFeedingSessionID == rootID }) { return rootID }
        guard let member = feeds.first(where: { $0.id == rootID }) else { throw FeedingSessionError.missingFeed }
        return member.effectiveFeedingSessionID
    }

    static func sleepSessionKey(_ rootID: UUID, in sleeps: [SleepEntry]) throws -> UUID {
        if sleeps.contains(where: { $0.effectiveSleepSessionID == rootID }) { return rootID }
        guard let member = sleeps.first(where: { $0.id == rootID }) else { throw SleepSessionError.missingSleep }
        return member.effectiveSleepSessionID
    }

    /// Tombstones `feed`, then, if its session is no longer keyed by a live
    /// member, makes the earliest one left the root and links the rest —
    /// and a running part that had joined — to it. A session with nothing
    /// left frees its running part.
    func deleteFeedPartLocked(_ feed: FeedEntry, among feeds: [FeedEntry]) throws {
        let session = feed.effectiveFeedingSessionID
        try tombstoneLocked(feed)
        let remaining = feeds.filter { $0.id != feed.id && $0.effectiveFeedingSessionID == session }
            .sorted(by: Self.feedOrder)
        guard !remaining.contains(where: { $0.id == session }) else { return }
        if let root = remaining.first {
            for member in remaining {
                let link: UUID? = member.id == root.id ? nil : root.id
                guard member.feedingSessionID != link else { continue }
                member.feedingSessionID = link
                member.lastModifiedAt = now
                try recordLocked(.update, on: member)
            }
        }
        try relinkRunningPartLocked(.feed, for: feed.babyID, from: session, to: remaining.first?.id)
    }

    /// `deleteFeedPartLocked` for a sleep.
    func deleteSleepPartLocked(_ sleep: SleepEntry, among sleeps: [SleepEntry]) throws {
        let session = sleep.effectiveSleepSessionID
        try tombstoneLocked(sleep)
        let remaining = sleeps.filter { $0.id != sleep.id && $0.effectiveSleepSessionID == session }
            .sorted(by: Self.sleepOrder)
        guard !remaining.contains(where: { $0.id == session }) else { return }
        if let root = remaining.first {
            for member in remaining {
                let link: UUID? = member.id == root.id ? nil : root.id
                guard member.sleepSessionID != link else { continue }
                member.sleepSessionID = link
                member.lastModifiedAt = now
                try recordLocked(.update, on: member)
            }
        }
        try relinkRunningPartLocked(.sleep, for: sleep.babyID, from: session, to: remaining.first?.id)
    }

    /// `softDelete`, for callers already inside the write boundary.
    func tombstoneLocked(_ entry: some ChangeLoggable & Touchable) throws {
        entry.isTombstoned = true
        entry.lastModifiedAt = now
        try recordLocked(.delete, on: entry)
    }

    /// Moves the running timer of `kind` off session `old`, the way
    /// `detachFromSession` does: a session-only edit naming no components.
    func relinkRunningPartLocked(_ kind: SessionKind, for babyID: UUID, from old: UUID, to new: UUID?) throws {
        guard let timer = try activeTimer(kind: kind == .feed ? .feed : .sleep, for: babyID),
              timer.sessionID == old else { return }
        timer.sessionID = new
        try reduceTimerIntentLocked(.edit(timerID: timer.id, babyID: babyID, startedAt: nil, components: [], sessionID: new))
    }

    /// After a component is removed, the entry's span and kind follow the
    /// parts left. The span moves only when every timed part knows its
    /// start; otherwise an old untimed part's place is unknown and the span
    /// is left as it was.
    static func fitToComponents(_ feed: FeedEntry) {
        let timed = feed.components.filter { $0.breastSide != nil || $0.bottleContents != nil }
        if !timed.isEmpty, timed.allSatisfy({ $0.startedAt != nil }),
           let start = timed.compactMap(\.startedAt).min(),
           let end = timed.compactMap({ $0.endedAt ?? $0.startedAt }).max() {
            feed.startedAt = start
            feed.endedAt = end
        }
        let hasBreast = feed.components.contains { $0.breastSide != nil }
        let hasBottle = feed.components.contains { $0.bottleContents != nil }
        if feed.kind == .breast, !hasBreast, hasBottle { feed.kind = .bottle }
        if feed.kind == .bottle, !hasBottle, hasBreast { feed.kind = .breast }
    }
}
