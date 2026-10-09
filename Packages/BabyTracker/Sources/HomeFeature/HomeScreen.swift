import Foundation
import BabyCore
import BabyStore
import BabyUI

/// Everything Home draws, derived from one `HomeSnapshot` by a pure function.
///
/// Every value is a fact or a `Date`, never a formatted elapsed string: the
/// numbers that move are drawn by timelines over these dates, so the screen
/// itself only changes when the data does.
public struct HomeScreen: Equatable, Sendable {
    public var hero: Hero
    /// Time since the activities that are not the hero.
    public var status: [StatusItem]
    /// Today at a glance, in every state. Nil only before the first load.
    public var strip: TodayStrip?
    public var dock: Dock
    /// The running part's earlier parts, when it joined a session.
    public var joinChip: JoinChip?
    /// When a running sleep's clock counts from, shown while the feed is the
    /// hero (`sleepClock(_:session:)`).
    public var sleepChip: Date?
}

/// The one large number, in priority order: a running feed, a running sleep,
/// an open feed break, then time awake.
public enum Hero: Equatable, Sendable {
    case feedRunning(icon: BabyIcon, title: String, startedAt: Date)
    /// `clockFrom` is when the clock counts from — earlier than `since` by
    /// the awake gaps when the sleep continues the same sleep, whose whole
    /// time asleep it counts; `since` is when that sleep first began.
    case sleepRunning(clockFrom: Date, since: Date)
    case feedBreak(since: Date, parts: [PartSummary])
    /// Nil when no sleep has ever been logged.
    case awake(since: Date?)
}

/// An activity's icon and the moment its time-since counts from.
public struct StatusItem: Equatable, Sendable {
    public let tracker: Tracker
    public let since: Date
}

/// One finished part of a session: its icon and how long it ran.
public struct PartSummary: Equatable, Sendable {
    public let icon: BabyIcon
    public let seconds: Int
}

/// "Same feed · L 9:02": the earlier parts the running timer joined.
public struct JoinChip: Equatable, Sendable {
    public let kind: SessionKind
    public let previousParts: [PartSummary]
}

/// Midnight to now: sleep spans, feed starts and the joined-session totals.
/// Totals count saved entries only; a running part counts once it stops.
public struct TodayStrip: Equatable, Sendable {
    public let dayStart: Date
    /// Finished sleep parts. A running sleep is `runningSleepSince`.
    public let sleeps: [DateInterval]
    public let feeds: [Date]
    public let feedCount: Int
    public let sleepSeconds: Int
    public let diaperCount: Int
    /// Today's measured bottle volume, in millilitres.
    public let volumeML: Double
    /// The running parts' starts, drawn up to the strip's own "now" so they
    /// grow without Home reloading.
    public let runningSleepSince: Date?
    public let runningFeedSince: Date?

    /// The six-hourly marks strictly between midnight and `now`, by the
    /// clock: on a daylight-saving day "06" is still six o'clock, not six
    /// hours after midnight.
    public func hourMarks(now: Date, calendar: Calendar = .current) -> [Date] {
        [6, 12, 18].compactMap { calendar.date(bySettingHour: $0, minute: 0, second: 0, of: dayStart) }
            .filter { $0 > dayStart && $0 < now }
    }
}

public enum Dock: Equatable, Sendable {
    /// Idle or in a break: Left and Right, then Bottle, Sleep, Diaper, More.
    case start(suggested: BreastSide)
    case feedRunning(timerID: UUID, isBottle: Bool)
    case sleepRunning(timerID: UUID)
    case readOnly
}

public extension HomeScreen {
    /// `now` decides only which day the strip covers. Everything that ticks
    /// is a `Date` the views count from.
    @MainActor
    static func make(snapshot s: HomeSnapshot, suggested: BreastSide, canLog: Bool, now: Date) -> HomeScreen {
        let feedTimer = s.feedTimer
        let sleepTimer = s.sleepTimer

        let feedPartStart = feedTimer.map { $0.currentBottleStartedAt ?? $0.currentSideStartedAt }
        let hero: Hero
        if let timer = feedTimer, let feedPartStart {
            let icon: BabyIcon = timer.currentBottleStartedAt != nil ? .bottle
                : timer.currentSide == .right ? .breastRight : .breastLeft
            hero = .feedRunning(icon: icon, title: HomeCopy.partTitle(icon), startedAt: feedPartStart)
        } else if let timer = sleepTimer {
            hero = .sleepRunning(clockFrom: sleepClock(timer, session: s.runningSleepSession),
                                 since: s.runningSleepSession?.startedAt ?? timer.startedAt)
        } else if let open = s.feedBreak {
            hero = .feedBreak(since: open.endedAt, parts: parts(of: open, in: s))
        } else {
            hero = .awake(since: s.awakeSince)
        }

        // Read off the hero rather than the timers: a sleep that runs during
        // a feed break is the hero, and the feed then belongs here.
        let feedIsHero: Bool = switch hero {
        case .feedRunning, .feedBreak: true
        case .sleepRunning, .awake: false
        }
        var status: [StatusItem] = []
        if !feedIsHero, let lastFeed = s.lastFeed {
            let end = s.lastFeedingSession?.endedAt ?? lastFeed.endedAt ?? lastFeed.startedAt
            status.append(StatusItem(tracker: .feed, since: end))
        }
        if feedIsHero, sleepTimer == nil, let awake = s.awakeSince {
            status.append(StatusItem(tracker: .sleep, since: awake))
        }
        if let diaper = s.lastDiaper {
            status.append(StatusItem(tracker: .diaper, since: diaper.startedAt))
        }

        let joinChip: JoinChip? = if let session = s.runningFeedSession, feedTimer != nil {
            JoinChip(kind: .feed, previousParts: parts(of: session, in: s))
        } else if let session = s.runningSleepSession, feedTimer == nil, sleepTimer != nil {
            JoinChip(kind: .sleep, previousParts: [PartSummary(icon: .sleep, seconds: session.asleepSeconds)])
        } else {
            nil
        }

        let strip = TodayStrip(dayStart: Calendar.current.startOfDay(for: now), sleeps: s.todaySleepSpans,
                               feeds: s.todayFeedTimes, feedCount: s.todayFeedCount,
                               sleepSeconds: s.todaySleepSeconds, diaperCount: s.todayDiaperCount,
                               volumeML: s.todayVolumeML, runningSleepSince: sleepTimer?.startedAt,
                               runningFeedSince: feedPartStart)

        let dock: Dock = !canLog ? .readOnly
            : feedTimer.map { .feedRunning(timerID: $0.id, isBottle: $0.currentBottleStartedAt != nil) }
            ?? sleepTimer.map { .sleepRunning(timerID: $0.id) }
            ?? .start(suggested: suggested)

        return HomeScreen(hero: hero, status: status, strip: strip, dock: dock, joinChip: joinChip,
                          sleepChip: feedTimer != nil ? sleepTimer.map { sleepClock($0, session: s.runningSleepSession) } : nil)
    }

    /// When a running sleep's clock counts from: its own start, moved back by
    /// the time asleep in the earlier parts of the sleep it continues, so the
    /// clock reads the whole sleep — as the Live Activity's does
    /// (`BabyStore.earlierAsleepSeconds(of:)`).
    private static func sleepClock(_ timer: ActiveTimer, session: SleepSession?) -> Date {
        timer.startedAt.addingTimeInterval(-TimeInterval(session?.asleepSeconds ?? 0))
    }

    /// One summary per milk part, in session order. Solids never join a milk
    /// session, so they have no part here.
    @MainActor
    private static func parts(of session: FeedingSession, in s: HomeSnapshot) -> [PartSummary] {
        s.sessionMembers(session).flatMap { feed -> [PartSummary] in
            feed.components.compactMap { component in
                if let side = component.breastSide {
                    PartSummary(icon: icon(for: side), seconds: component.durationSeconds ?? 0)
                } else if component.bottleContents != nil {
                    PartSummary(icon: .bottle, seconds: component.bottleSeconds ?? 0)
                } else {
                    nil
                }
            }
        }
    }

    private static func icon(for side: BreastSide) -> BabyIcon {
        side == .left ? .breastLeft : .breastRight
    }
}
