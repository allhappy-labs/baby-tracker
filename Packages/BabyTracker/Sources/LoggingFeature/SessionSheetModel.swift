import Foundation
import Observation
import BabyCore
import BabyStore
import BabyUI

/// The session sheet's strings.
public enum SessionSheetCopy {
    public static let done = LoggingL10n.text("Done")
    public static let splitHere = LoggingL10n.text("Split here")
    public static let addPart = LoggingL10n.text("Add part")
    public static let deleteFeed = LoggingL10n.text("Delete feed")
    public static let deleteSleep = LoggingL10n.text("Delete sleep")
    public static let deleteFeedConfirmTitle = LoggingL10n.text("Delete this feed?")
    public static let deleteSleepConfirmTitle = LoggingL10n.text("Delete this sleep?")
    /// "52m later": from the end of one part to the start of the next. A
    /// length between two times, not an age.
    public static func later(_ gap: String) -> String { LoggingL10n.format("%@ later", gap) }
    /// What VoiceOver reads for Split here: which part the cut falls before.
    public static func splitBefore(_ part: String) -> String { LoggingL10n.format("Split here, before %@", part) }
    /// "27m breast" in a feed's totals line.
    public static func breast(_ length: String) -> String { LoggingL10n.format("%@ breast", length) }
    /// "1 wake" / "2 wakes" in a sleep's totals line: the gaps between its parts.
    public static func wakes(_ count: Int) -> String {
        count == 1 ? LoggingL10n.format("%lld wake", count) : LoggingL10n.format("%lld wakes", count)
    }
}

/// One part of a session as the session sheet lists it: a breast or bottle
/// component of one of the session's feeds, or one of its sleeps (ruling H-R2).
public struct SessionPart: Identifiable, Equatable {
    /// The component's id, or the sleep's.
    public let id: UUID
    public let target: PartEditTarget
    public let icon: BabyIcon
    /// "Left", "Right", "Formula 60 ml", or a sleep's length, "1h 2m".
    public let label: String
    /// "10:23 · 12m"; a bottle's time alone; a sleep's "21:10 – 22:12".
    public let detail: String
    /// "52m later". Nil for the first part, and between two parts of an old
    /// combined feed whose components never recorded their times.
    public let gapBefore: String?
    /// True only when the part before belongs to a different entry: Split
    /// separates entries, and cannot cut an old combined feed (ruling H-R2).
    public let canSplitBefore: Bool
}

/// What "Add part" adds. Solids join a feed only this way: the break limit
/// never joins them.
public enum AddPartKind: CaseIterable, Sendable {
    case left, right, bottle, solids, sleep

    /// What a session of `kind` offers.
    public static func offered(for kind: SessionKind) -> [AddPartKind] {
        allCases.filter { $0.sessionKind == kind }
    }

    public var sessionKind: SessionKind { self == .sleep ? .sleep : .feed }

    public var title: String {
        switch self {
        case .left: BreastSide.left.displayName
        case .right: BreastSide.right.displayName
        case .bottle: PartEditCopy.bottleTitle
        case .solids: SolidsSheetCopy.title
        case .sleep: Tracker.sleep.title
        }
    }

    public var icon: BabyIcon {
        switch self {
        case .left: .breastLeft
        case .right: .breastRight
        case .bottle: .bottle
        case .solids: .solids
        case .sleep: .sleep
        }
    }
}

/// A feed or sleep session as one sheet: its time and totals, the Join row,
/// its parts with the gap between each, and Split here, Add part and Delete.
///
/// Holds ids, not models: the other phone may change the session while the
/// sheet is open, so every write names entries afresh, and `reload()`, which
/// the sheet calls on every `store.revision`, rebuilds what it shows.
///
/// The session it shows is found again on every reload, and `rootID`
/// follows it: after Join it is the earlier session's root, after the first
/// part is deleted the next part's id, and after Split the earlier half's.
@MainActor @Observable
public final class SessionSheetModel {
    let store: BabyStore
    let babyID: UUID
    public let kind: SessionKind
    public let unit: VolumeUnit

    /// The shown session's own id (`FeedingSession.id` / `SleepSession.id`):
    /// its first part's, or a deleted entry's when an older phone tombstoned
    /// the first part without relinking the rest.
    public private(set) var rootID: UUID
    /// "10:23 – 11:48".
    public private(set) var title = ""
    /// "1h 25m · 27m breast · 60 ml", or "2h 9m · 1 wake".
    public private(set) var totals = ""
    public private(set) var parts: [SessionPart] = []
    /// Replaced when the session's root changes, so the row always offers
    /// the session before the one shown — until it has joined once, after
    /// which it stays hidden while the sheet is open (final review I3).
    public private(set) var join: JoinRowModel
    /// True once the session no longer exists: every part deleted, here or
    /// on the other phone. The sheet closes on it.
    public private(set) var isGone = false
    /// False for a view-only caregiver (ruling H-R6): the sheet then offers
    /// no Join, Split, Add, Delete, swipe or part editor. Read on reload, as
    /// a role change arrives as a revision like any other.
    public private(set) var canEdit = true
    /// The session's last moment, where Add part starts.
    private var endedAt: Date?
    /// The latest start among the members. An added part starts strictly
    /// after it (ruling H-R17): a tie on start is broken by id, which could
    /// list the new part above a zero-length bottle at the session's end and
    /// make Split cut in the wrong place, or write nothing.
    private var latestMemberStart: Date?
    /// Every member as last read, in session order: if the root goes, the
    /// next one still finds the session.
    private var memberIDs: [UUID] = []

    public init(store: BabyStore, babyID: UUID, kind: SessionKind, rootID: UUID, unit: VolumeUnit = .millilitres) {
        self.store = store
        self.babyID = babyID
        self.kind = kind
        self.rootID = rootID
        self.unit = unit
        join = JoinRowModel(store: store, babyID: babyID, kind: kind, rootID: rootID)
        reload()
    }

    /// Re-reads the session after any write, this phone's or the other's.
    public func reload() {
        canEdit = store.canWriteInActiveRealm
        let found: (id: UUID, members: [UUID], startedAt: Date, endedAt: Date?)?
        switch kind {
        case .feed:
            found = findSession(in: (try? store.feedingSessions(for: babyID, from: .distantPast, to: .distantFuture)) ?? [],
                                id: \.id, members: \.feedIDs)
                .map { ($0.id, $0.feedIDs, $0.startedAt, $0.endedAt) }
        case .sleep:
            found = findSession(in: (try? store.sleepSessions(for: babyID, from: .distantPast, to: .distantFuture)) ?? [],
                                id: \.id, members: \.sleepIDs)
                .map { ($0.id, $0.sleepIDs, $0.startedAt, $0.endedAt) }
        }
        guard let found else {
            isGone = true
            parts = []
            memberIDs = []
            return
        }
        isGone = false
        rootID = found.id
        memberIDs = found.members
        switch kind {
        case .feed: readFeeds(found.members, startedAt: found.startedAt, endedAt: found.endedAt ?? found.startedAt)
        case .sleep: readSleeps(found.members, startedAt: found.startedAt, endedAt: found.endedAt)
        }
        if join.rootID == rootID {
            join.reload()
        } else {
            join = JoinRowModel(store: store, babyID: babyID, kind: kind, rootID: rootID, hasJoined: join.hasJoined)
        }
    }

    /// Starts a new session at the part's entry; every later part goes with
    /// it (`separateFeed(at:)` / `separateSleep(at:)`). Refused between two
    /// components of one entry, which cannot be split (ruling H-R2).
    public func split(before partID: UUID) throws {
        guard let part = parts.first(where: { $0.id == partID }) else { throw EditModeError.gone }
        guard part.canSplitBefore else { throw EditModeError.invalid }
        switch kind {
        case .feed: try store.separateFeed(at: part.target.entryID, for: babyID)
        case .sleep: try store.separateSleep(at: part.target.entryID, for: babyID)
        }
        reload()
    }

    /// Whether Add part can add `addition` now. Added parts never end in the
    /// future (ruling H-R18), so a breast side waits until the session ended
    /// at least the store's 30-second minimum ago, and a sleep a minute.
    public func canAdd(_ addition: AddPartKind) -> Bool {
        guard addition.sessionKind == kind, !isGone, let start = addedPartStart else { return false }
        return start.addingTimeInterval(Self.minimumLength(addition)) <= addedPartLimit
    }

    /// The Solids panel for solids eaten during this feed, at the session's
    /// end to start with. Nothing is written until it saves, as solids are
    /// their foods and there is no sensible default to write first.
    public func newSolidsModel() -> SolidsLogModel? {
        guard kind == .feed, canAdd(.solids), let start = addedPartStart else { return nil }
        return SolidsLogModel(store: store, babyID: babyID, session: rootID, at: start)
    }

    /// Adds a part starting at the session's end, linked to this session
    /// whatever the break limit says, and returns it for its editor to open.
    /// It starts a second later when a part already starts at that end
    /// (ruling H-R17), so it always lists last. A breast side or sleep lasts
    /// ten minutes, as on the Past entry sheet, or up to now when the session
    /// ended less than that ago (ruling H-R18), or up to where a part running
    /// on from the session began (final review M6); a bottle takes the Bottle
    /// sheet's default amount and contents. Refused while `canAdd` is false,
    /// and for solids, which `newSolidsModel()` adds once they are named.
    public func addPart(_ addition: AddPartKind) throws -> PartEditTarget {
        guard addition.sessionKind == kind, addition != .solids else { throw EditModeError.invalid }
        guard !isGone, let start = addedPartStart else { throw EditModeError.gone }
        guard canAdd(addition) else { throw EditModeError.invalid }
        let end = min(start.addingTimeInterval(600), addedPartLimit)
        let target: PartEditTarget
        switch addition {
        case .left, .right:
            let feed = try store.addFeedPart(.breast(side: addition == .left ? .left : .right, from: start, to: end),
                                             toSession: rootID, for: babyID)
            guard let component = feed.components.first else { throw EditModeError.gone }
            target = .breast(feedID: feed.id, componentID: component.id)
        case .bottle:
            let defaults = LogDefaults(store: store)
            let volume = (try? defaults.lastBottleVolumeML(for: babyID)) ?? LogDefaults.fallbackVolumeML
            let contents = (try? defaults.lastBottleContents(for: babyID)) ?? LogDefaults.fallbackContents
            let feed = try store.addFeedPart(.bottle(volumeML: volume, contents: contents, at: start),
                                             toSession: rootID, for: babyID)
            guard let component = feed.components.first else { throw EditModeError.gone }
            target = .bottle(feedID: feed.id, componentID: component.id)
        case .solids:
            throw EditModeError.invalid
        case .sleep:
            let sleep = try store.addSleepPart(from: start, to: end, toSession: rootID, for: babyID)
            target = .sleep(sleepID: sleep.id)
        }
        reload()
        return target
    }

    /// Removes one part the session-aware way (rulings H-R2, H-R11): a
    /// component comes out of its entry, the entry goes with its last one,
    /// and a deleted first part makes the next the root. Deleting the last
    /// part leaves the session gone.
    public func deletePart(_ partID: UUID) throws {
        guard let part = parts.first(where: { $0.id == partID }) else { return reload() }
        switch part.target {
        case let .breast(feedID, componentID), let .bottle(feedID, componentID), let .solids(feedID, componentID):
            try store.removeComponent(componentID, fromFeed: feedID, for: babyID)
        case let .sleep(sleepID):
            try store.deleteSleepPart(sleepID, for: babyID)
        }
        reload()
    }

    /// Tombstones every part of the session in one write.
    public func deleteSession() throws {
        guard !isGone else { throw EditModeError.gone }
        try store.deleteSession(kind, rootID: rootID, for: babyID)
        reload()
    }

    /// The editor a part row pushes, inside this sheet.
    public func editorModel(for target: PartEditTarget) -> PartEditModel {
        PartEditModel(store: store, babyID: babyID, target: target, unit: unit)
    }

    // MARK: - Private

    /// The session to show: the one keyed by `rootID`, else the one holding
    /// the earliest member last seen that is still there — or, on the first
    /// read, before any was seen, the one holding `rootID`.
    ///
    /// The key comes first, and `rootID` stops counting as a member once
    /// members were seen: parts that continued a joined-away session can be
    /// keyed by an entry that is now a member of an earlier session (final
    /// review I2). The sheet shows the parts keyed by it, and once they are
    /// gone it is gone, rather than turning into that earlier session.
    private func findSession<Session>(in sessions: [Session], id: KeyPath<Session, UUID>,
                                      members: KeyPath<Session, [UUID]>) -> Session? {
        if let keyed = sessions.first(where: { $0[keyPath: id] == rootID }) { return keyed }
        for candidate in memberIDs.isEmpty ? [rootID] : memberIDs {
            if let session = sessions.first(where: { $0[keyPath: members].contains(candidate) }) {
                return session
            }
        }
        return nil
    }

    /// Where an added part starts: the session's end, or a second after the
    /// latest member's start when one starts there (ruling H-R17).
    private var addedPartStart: Date? {
        guard let endedAt else { return nil }
        guard let latestMemberStart, latestMemberStart >= endedAt else { return endedAt }
        return latestMemberStart.addingTimeInterval(1)
    }

    /// Where an added part must end by: now, or earlier where a part still
    /// running on from this session began (final review M6) — it started
    /// after the session's end, and an added part running under it would be
    /// counted twice once it stops.
    private var addedPartLimit: Date {
        let timerKind: TimerKind = kind == .feed ? .feed : .sleep
        guard let timer = try? store.activeTimer(kind: timerKind, for: babyID), timer.sessionID == rootID else {
            return store.now
        }
        return min(store.now, timer.startedAt)
    }

    /// The shortest part Add part may write: the store's breast minimum, a
    /// minute of sleep, and a bottle's or solids' moment.
    private static func minimumLength(_ addition: AddPartKind) -> TimeInterval {
        switch addition {
        case .left, .right: 30
        case .sleep: 60
        case .bottle, .solids: 0
        }
    }

    private func readFeeds(_ ids: [UUID], startedAt: Date, endedAt: Date) {
        let feeds: [FeedEntry] = ids.compactMap {
            guard case .feed(let feed)? = try? store.entry(id: $0, for: babyID) else { return nil }
            return feed
        }
        let zone = feeds.first?.timeZoneID ?? TimeZone.current.identifier
        title = Self.range(startedAt, endedAt, zone: zone)

        var parts: [SessionPart] = []
        // The part before: its entry, its own end when known, and its
        // entry's end, which still dates the gap to the next entry when an
        // old combined feed never recorded its parts' times.
        var previous: (entryID: UUID, end: Date?, entryEnd: Date)?
        var breastSeconds = 0
        var volumeML = 0.0
        for feed in feeds {
            for component in feed.components {
                // A single-part feed's time is the feed's; a part of an old
                // combined feed may not know its own.
                let timed = feed.components.count == 1 || component.startedAt != nil
                let start = feed.components.count == 1 ? feed.startedAt : component.startedAt ?? feed.startedAt
                let label: String
                let detail: String
                let icon: BabyIcon
                let target: PartEditTarget
                let end: Date
                switch component {
                case let .breast(id, side, seconds, _, componentEnd):
                    label = side.displayName
                    icon = side == .left ? .breastLeft : .breastRight
                    target = .breast(feedID: feed.id, componentID: id)
                    end = feed.components.count == 1
                        ? feed.endedAt ?? start.addingTimeInterval(TimeInterval(seconds))
                        : componentEnd ?? start.addingTimeInterval(TimeInterval(seconds))
                    detail = "\(Self.clock(start, zone: feed.timeZoneID))–\(Self.clock(end, zone: feed.timeZoneID))"
                    breastSeconds += seconds
                case let .bottle(id, volume, contents, seconds, _, componentEnd):
                    label = "\(contents.displayName) \(Volume(millilitres: volume).formatted(as: unit))"
                    icon = .bottle
                    target = .bottle(feedID: feed.id, componentID: id)
                    end = feed.components.count == 1
                        ? feed.endedAt ?? start
                        : componentEnd ?? start.addingTimeInterval(TimeInterval(seconds ?? 0))
                    let time = Self.clock(start, zone: feed.timeZoneID)
                    detail = (seconds ?? 0) > 0 ? "\(time)–\(Self.clock(end, zone: feed.timeZoneID))" : time
                    volumeML += volume
                case let .solids(id, foods, amount):
                    label = Self.solidsLabel(foods, amount)
                    icon = .solids
                    target = .solids(feedID: feed.id, componentID: id)
                    end = start
                    detail = Self.clock(start, zone: feed.timeZoneID)
                }
                // An entry's first part starts with the entry, timed or not.
                let startsEntry = previous.map { $0.entryID != feed.id } ?? false
                let previousEnd = previous.flatMap { $0.end ?? (startsEntry ? $0.entryEnd : nil) }
                let gap: String? = if let previousEnd, timed || startsEntry {
                    SessionSheetCopy.later(ElapsedFormatter.total(Int(start.timeIntervalSince(previousEnd))))
                } else {
                    nil
                }
                parts.append(SessionPart(id: component.id, target: target, icon: icon, label: label, detail: detail,
                                         gapBefore: gap,
                                         canSplitBefore: startsEntry))
                previous = (feed.id, timed ? end : nil, feed.endedAt ?? feed.startedAt)
            }
        }
        self.parts = parts
        self.endedAt = endedAt
        latestMemberStart = feeds.map(\.startedAt).max()

        var totals: [String] = []
        let length = Int(endedAt.timeIntervalSince(startedAt))
        if length > 0 { totals.append(ElapsedFormatter.total(length)) }
        if breastSeconds > 0 { totals.append(SessionSheetCopy.breast(ElapsedFormatter.total(breastSeconds))) }
        if volumeML > 0 { totals.append(Volume(millilitres: volumeML).formatted(as: unit)) }
        self.totals = totals.joined(separator: " · ")
    }

    private func readSleeps(_ ids: [UUID], startedAt: Date, endedAt: Date?) {
        let sleeps: [SleepEntry] = ids.compactMap {
            guard case .sleep(let sleep)? = try? store.entry(id: $0, for: babyID) else { return nil }
            return sleep
        }
        let zone = sleeps.first?.timeZoneID ?? TimeZone.current.identifier
        // An open sleep has no session end; its latest known moment stands in.
        let lastMoment = endedAt ?? sleeps.map { $0.endedAt ?? $0.startedAt }.max() ?? startedAt
        title = endedAt.map { Self.range(startedAt, $0, zone: zone) } ?? Self.clock(startedAt, zone: zone)

        var parts: [SessionPart] = []
        var previousEnd: Date?
        for sleep in sleeps {
            let start = Self.clock(sleep.startedAt, zone: sleep.timeZoneID)
            let label = sleep.endedAt.map { ElapsedFormatter.total(Int($0.timeIntervalSince(sleep.startedAt))) } ?? start
            let detail = sleep.endedAt.map { "\(start) – \(Self.clock($0, zone: sleep.timeZoneID))" } ?? start
            let gap = previousEnd.map { SessionSheetCopy.later(ElapsedFormatter.total(Int(sleep.startedAt.timeIntervalSince($0)))) }
            parts.append(SessionPart(id: sleep.id, target: .sleep(sleepID: sleep.id), icon: .sleep, label: label,
                                     detail: detail, gapBefore: gap, canSplitBefore: !parts.isEmpty))
            previousEnd = sleep.endedAt ?? sleep.startedAt
        }
        self.parts = parts
        self.endedAt = lastMoment
        latestMemberStart = sleeps.map(\.startedAt).max()

        var totals = [ElapsedFormatter.total(sleeps.compactMap(\.durationSeconds).reduce(0, +))]
        if sleeps.count > 1 { totals.append(SessionSheetCopy.wakes(sleeps.count - 1)) }
        self.totals = totals.joined(separator: " · ")
    }

    /// "Pear, oat · A little", as History's chip reads; the amount alone
    /// when no food was named.
    static func solidsLabel(_ foods: [String], _ amount: SolidsAmount) -> String {
        ((foods.isEmpty ? [] : [foods.joined(separator: ", ").sentenceCased]) + [amount.displayName])
            .joined(separator: " · ")
    }

    /// "10:23 – 11:48", or the one time when the session is a moment.
    private static func range(_ start: Date, _ end: Date, zone: String) -> String {
        let from = clock(start, zone: zone)
        let to = clock(end, zone: zone)
        return from == to ? from : "\(from) – \(to)"
    }

    private static func clock(_ date: Date, zone: String) -> String {
        JoinRowModel.clockTime(date, zone: zone)
    }
}
