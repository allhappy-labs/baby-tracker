import Foundation
import BabyCore

/// Everything Home needs, assembled in one pass so the view does no querying.
@MainActor
public struct HomeSnapshot {
    public let lastFeed: FeedEntry?
    public let lastFeedingSession: FeedingSession?
    public let lastSleep: SleepEntry?
    public let lastDiaper: DiaperEntry?
    public let runningTimers: [ActiveTimer]
    public let todayFeedCount: Int
    public let todayVolumeML: Double
    /// The sum of `asleepSeconds` over the sleep sessions whose first part
    /// started today — a session formed by joining across midnight counts
    /// in full on the day its first part began, never split or double-counted.
    public let todaySleepSeconds: Int
    public let todayDiaperCount: Int

    /// The last feed session, while no feed timer runs and the gap since it
    /// ended is still within the feed break limit. Nil once a feed timer is
    /// running (its earlier parts show as `runningFeedSession` instead) or
    /// once the gap has passed the limit.
    public let feedBreak: FeedingSession?
    /// The earlier parts the running feed timer joins, when it joined one.
    public let runningFeedSession: FeedingSession?
    /// The earlier parts the running sleep timer joins, when it joined one.
    public let runningSleepSession: SleepSession?
    /// The end of the last completed sleep part, regardless of break limits.
    public let awakeSince: Date?
    public let breakLimits: BreakLimits
    /// Today's finished sleep parts, clipped to today's bounds. A running
    /// sleep is `sleepTimer`, which the today strip draws live.
    public let todaySleepSpans: [DateInterval]
    /// The start of each of today's feed parts, for the today strip.
    public let todayFeedTimes: [Date]
    /// The members of the sessions this snapshot exposes — the last feeding
    /// session, the open feed break and the running feed's session — keyed by
    /// id, so `sessionMembers(_:)` resolves them without another fetch.
    let sessionFeedsByID: [UUID: FeedEntry]
    /// The family's running pump, the same on every baby's Home.
    public var runningPump: PumpEntry? = nil
    /// Millilitres pumped in finished sessions that started today.
    public var todayPumpML: Double = 0
    /// When the last finished session ended, for "last 2h 27m ago".
    public var lastPumpEndedAt: Date? = nil

    /// A session's feeds in session order, for those this snapshot exposes:
    /// `lastFeedingSession`, `feedBreak` and `runningFeedSession`. Any other
    /// session resolves to whatever of it the snapshot happens to hold.
    public func sessionMembers(_ session: FeedingSession) -> [FeedEntry] {
        session.feedIDs.compactMap { sessionFeedsByID[$0] }
    }

    public var feedTimer: ActiveTimer? {
        runningTimers.first { $0.kind == .feed }
    }

    public var sleepTimer: ActiveTimer? {
        runningTimers.first { $0.kind == .sleep }
    }

    /// The timer the dial shows: the most recently started one.
    public var primaryTimer: ActiveTimer? {
        runningTimers.first
    }

    public var isAsleep: Bool {
        sleepTimer != nil
    }
}

/// One row in History, whichever kind it is.
@MainActor
public enum HistoryItem: @MainActor Identifiable {
    case feed(FeedEntry)
    case sleep(SleepEntry)
    case diaper(DiaperEntry)
    case supplement(SupplementEntry)
    case immunization(ImmunizationEntry)
    case pump(PumpEntry)

    public var id: UUID {
        switch self {
        case .feed(let entry): entry.id
        case .sleep(let entry): entry.id
        case .diaper(let entry): entry.id
        case .supplement(let entry): entry.id
        case .immunization(let entry): entry.id
        case .pump(let entry): entry.id
        }
    }

    public var startedAt: Date {
        switch self {
        case .feed(let entry): entry.startedAt
        case .sleep(let entry): entry.startedAt
        case .diaper(let entry): entry.startedAt
        case .supplement(let entry): entry.startedAt
        case .immunization(let entry): entry.givenAt
        case .pump(let entry): entry.startedAt
        }
    }

    public var timeZoneID: String {
        switch self {
        case .feed(let entry): entry.timeZoneID
        case .sleep(let entry): entry.timeZoneID
        case .diaper(let entry): entry.timeZoneID
        case .supplement(let entry): entry.timeZoneID
        case .immunization(let entry): entry.timeZoneID
        case .pump(let entry): entry.timeZoneID
        }
    }
}
