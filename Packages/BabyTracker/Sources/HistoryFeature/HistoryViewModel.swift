import Foundation
import Observation
import BabyCore
import BabyStore
import BabyUI

/// Where tapping a row leads. The view hands it on as a `HistoryAction`;
/// HistoryFeature never presents an editor itself.
public enum HistoryRowTarget: Equatable {
    /// One entry, opened in the sheet it was logged with (edit mode).
    case entry(EntryRef)
    /// A feed or sleep with more than one part, opened in the session sheet.
    case session(SessionKind, rootID: UUID)
    /// A running feed or sleep, opened in the running-timer editor.
    case runningTimer(UUID)
}

/// One part of a feed, as a chip: "L 12m", "60 ml", "Pear · Some".
public struct HistoryChip: Equatable, Identifiable {
    /// The chip's position in its row.
    public let id: Int
    public let icon: BabyIcon?
    public let text: String
}

/// One line of History: a whole feed or sleep session, or one entry.
public struct HistoryListRow: Identifiable, Equatable {
    public let id: UUID
    public let tracker: Tracker
    /// Clock time in the entry's own zone, e.g. "10:23".
    public let start: String
    /// Shown under the start: a clock time, "now" while running, or nil
    /// when the entry has no length (a diaper, a manually logged bottle).
    public let end: String?
    /// Feeds only: every part of every member, in order.
    public let chips: [HistoryChip]
    /// Sleeps "2h 9m · 1 wake", diapers "Wet", meds the name, growth the values.
    public let text: String?
    public let isRunning: Bool
    /// Running rows only (H-R20): what the live duration counts from — the
    /// same moment as Home's hero, the current part's start for a feed and
    /// the timer's start for a sleep. Nil on every finished row.
    public let runningSince: Date?
    public let target: HistoryRowTarget
    /// Deleting a session removes several parts, so it asks first; a single
    /// entry deletes straight from the swipe.
    public let deleteConfirmation: Bool
}

/// A day header's icon totals. Zero / empty means "not shown": the tracker
/// had nothing that day, or the filter hides it.
public struct HistoryDayTotals: Equatable, Sendable {
    /// Feeding sessions that started that day, counted once however many
    /// parts they have — the same number as Home's `todayFeedCount`, solids
    /// sessions included.
    public let feeds: Int
    /// The asleep total of the sleep sessions that started that day, or ""
    /// when there were none.
    public let sleep: String
    /// Every bottle part of the feeding sessions that started that day, in
    /// millilitres — the day the feed count uses, so a bottle after midnight
    /// stays with the feed it belongs to. Formatted where it is shown.
    public let bottleML: Double
    public let diapers: Int
}

public struct HistoryDay: Identifiable, Equatable {
    public let id: DayKey
    /// "Today", "Yesterday", then the abbreviated date (H-R10).
    public let title: String
    public let totals: HistoryDayTotals
    public let rows: [HistoryListRow]
}

/// Why the list is empty. "Nothing logged yet" is true only when the store
/// itself is empty; saying it while a filter is hiding entries tells the
/// user their data is gone.
public enum HistoryEmptyState: Equatable {
    /// Nothing has been logged for this baby.
    case noData
    /// The filter hides everything there is; clearing it brings rows back.
    case nothingMatches
    /// Everything is shown, but every entry is older than the loaded days.
    case nothingRecent(dayCount: Int)
}

public enum HistoryDeleteError: Error, Equatable {
    /// A running timer is stopped or discarded in its own editor, not
    /// deleted from the list.
    case runningTimer
}

@MainActor
@Observable
public final class HistoryViewModel {
    /// Every tracker History lists, in filter-sheet order. Solids are
    /// filtered as feeds.
    public static let trackers: [Tracker] = [.feed, .sleep, .diaper, .supplement, .growth, .vaccine, .pump]

    private let store: BabyStore
    private let babyID: UUID
    /// A preference, not a fact about the entries: it can change while this
    /// screen is alive, and every volume on it is formatted here. The view
    /// sets it and reloads.
    public var volumeUnit: VolumeUnit

    public var filter: Set<Tracker> = Set(HistoryViewModel.trackers)
    public var isFiltered: Bool { filter != Set(Self.trackers) }

    /// What a focus asks of the list: its filter (or all trackers), and the
    /// entry to open, if any.
    public func apply(_ focus: HistoryFocus) -> UUID? {
        filter = focus.filter ?? Set(Self.trackers)
        return focus.entryID
    }

    private var handledFocusID: UUID?

    /// Applies a focus once, and returns it so the view can open its entry
    /// and tell the app it is spent. Nil — no focus, or one already applied —
    /// asks nothing: the app clears its focus once it is handled, and that
    /// change must not touch a filter the parent has since changed.
    public func consume(_ focus: HistoryFocus?) -> HistoryFocus? {
        guard let focus, focus.id != handledFocusID else { return nil }
        handledFocusID = focus.id
        _ = apply(focus)
        return focus
    }

    public private(set) var days: [HistoryDay] = []
    /// False for a view-only caregiver (H-R6): rows are neither tappable nor
    /// swipeable.
    public private(set) var canEdit = true
    /// Non-nil exactly when `days` is empty.
    public private(set) var emptyState: HistoryEmptyState?

    /// How many days one page adds as the list nears its end (ruling H-R23).
    public static let pageSize = 14

    /// The window of the last load, in days back from today, so a reload or
    /// a delete keeps every page already scrolled in.
    public private(set) var dayCount = HistoryViewModel.pageSize
    /// True while older days than the window may hold something: the window
    /// has not yet reached the baby's birth day, nor anything logged before
    /// it. The list pages in more while this holds.
    public private(set) var hasMore = false

    public init(store: BabyStore, babyID: UUID, volumeUnit: VolumeUnit = .millilitres) {
        self.store = store
        self.babyID = babyID
        self.volumeUnit = volumeUnit
    }

    // MARK: - Load

    /// Re-reads the days already loaded.
    public func reload() throws {
        try load(dayCount: dayCount)
    }

    /// Adds the next page of older days. Does nothing once there is nothing
    /// older to show.
    public func loadMore() throws {
        guard hasMore else { return }
        try load(dayCount: dayCount + Self.pageSize)
    }

    public func load(dayCount: Int) throws {
        self.dayCount = max(dayCount, 1)
        canEdit = store.canWriteInActiveRealm

        let zone = TimeZone.current.identifier
        let calendar = Calendar.current
        let dayKeys = (0..<self.dayCount).compactMap { offset in
            calendar.date(byAdding: .day, value: -offset, to: store.now)
                .map { DayBucket.key(for: $0, timeZoneID: zone) }
        }
        let today = DayBucket.key(for: store.now, timeZoneID: zone)
        let yesterday = calendar.date(byAdding: .day, value: -1, to: store.now)
            .map { DayBucket.key(for: $0, timeZoneID: zone) }
        let builder = HistoryDayBuilder(store: store, babyID: babyID, volumeUnit: volumeUnit, zone: zone)
        let built = try builder.build(from: dayKeys.last ?? today, through: today, filter: filter)

        let timers = try store.activeTimers(for: babyID)
        let runningPump = filter.contains(.pump) ? try store.runningPump().map(runningPumpRow) : nil
        let running = timers
            .filter { filter.contains($0.kind == .feed ? .feed : .sleep) }
            .map { runningRow($0, builder: builder) }
            + (runningPump.map { [$0] } ?? [])

        days = dayKeys.compactMap { key in
            let day = built.days[key]
            let rows = (key == today ? running : []) + (day?.entries.map(\.row) ?? [])
            guard !rows.isEmpty else { return nil }
            return HistoryDay(
                id: key,
                title: Self.title(for: key, today: today, yesterday: yesterday, zone: zone),
                totals: day?.totals ?? .none,
                rows: rows
            )
        }

        hasMore = try built.windowStart > oldestMoment(measurements: built.measurements)

        if !days.isEmpty {
            emptyState = nil
        } else if !(try hasAnyData(items: built.items, measurements: built.measurements, timers: timers)) {
            emptyState = .noData
        } else {
            emptyState = isFiltered ? .nothingMatches : .nothingRecent(dayCount: self.dayCount)
        }
    }

    /// The earliest moment History may have to reach back to: the start of
    /// the baby's birth day, or anything logged before it (a birth date set
    /// later than the first entries). Bounds the paging; with no baby to
    /// read, there is nothing older to page in.
    private func oldestMoment(measurements: [MeasurementCheckIn]) throws -> Date {
        guard let birthDate = try store.baby(babyID)?.birthDate else { return .distantFuture }
        let zone = TimeZone.current.identifier
        let birthDay = DayBucket.bounds(of: DayBucket.key(for: birthDate, timeZoneID: zone), timeZoneID: zone).start
        let beforeBirth = try store.entries(for: babyID, from: .distantPast, to: birthDay).map { item -> Date in
            switch item {
            case .feed(let feed): feed.startedAt
            case .sleep(let sleep): sleep.startedAt
            case .diaper(let diaper): diaper.startedAt
            case .supplement(let med): med.startedAt
            case .immunization(let shot): shot.givenAt
            case .pump(let pump): pump.startedAt
            }
        }
        return (beforeBirth + measurements.map(\.measuredAt) + [birthDay]).min() ?? birthDay
    }

    /// Whether anything at all is logged, however old. The window alone
    /// cannot say: a store whose every entry is older than it is not empty.
    private func hasAnyData(items: [HistoryItem], measurements: [MeasurementCheckIn], timers: [ActiveTimer]) throws -> Bool {
        guard items.isEmpty, measurements.isEmpty, timers.isEmpty else { return true }
        return try store.lastFeed(for: babyID) != nil
            || store.lastSleep(for: babyID) != nil
            || store.lastDiaper(for: babyID) != nil
            || store.lastSupplement(for: babyID) != nil
            || store.lastImmunization(for: babyID) != nil
            || store.lastFinishedPump() != nil
            || store.runningPump() != nil
    }

    // MARK: - Focus

    /// Names the entry an Ask source points at, whatever its kind, so its
    /// edit sheet can open (ruling H-R9). Nil when it is gone or never was
    /// this baby's.
    public func entryRef(for entryID: UUID) throws -> EntryRef? {
        switch try store.entry(id: entryID, for: babyID) {
        case .feed(let feed)?: return .feed(feed.id)
        case .sleep(let sleep)?: return .sleep(sleep.id)
        case .diaper(let diaper)?: return .diaper(diaper.id)
        case .supplement(let med)?: return .supplement(med.id)
        case .immunization(let shot)?: return .immunization(shot.id)
        case .pump(let pump)?: return .pump(pump.id)
        case nil:
            // Growth isn't a History entry in the store (H-R1).
            return try store.measurements(for: babyID).contains { $0.id == entryID } ? .measurement(entryID) : nil
        }
    }

    // MARK: - Delete

    /// Deletes a row the parent confirmed, as `delete`, if the list still
    /// shows it as it was swiped. The confirmation holds the swiped row: had
    /// the other phone since joined that session into an earlier one, its
    /// id would now name more than was shown. Returns false, deleting
    /// nothing, when the row is gone or now opens something else.
    @discardableResult
    public func deleteConfirmed(_ row: HistoryListRow) throws -> Bool {
        try reload()
        guard let current = days.lazy.flatMap(\.rows).first(where: { $0.id == row.id }),
              current.target == row.target else { return false }
        try delete(current)
        return true
    }

    /// A session row tombstones every member; a single row deletes its one
    /// entry. Feeds and sleeps go through the store's part calls so a joined
    /// session is never left keyed by a deleted root (H-R11).
    public func delete(_ row: HistoryListRow) throws {
        switch row.target {
        case let .session(kind, rootID):
            try store.deleteSession(kind, rootID: rootID, for: babyID)
        case .entry(.feed(let id)):
            try store.deleteFeedPart(id, for: babyID)
        case .entry(.sleep(let id)):
            try store.deleteSleepPart(id, for: babyID)
        case .entry(.diaper(let id)), .entry(.supplement(let id)):
            // Already gone (deleted on the other phone): the reload below
            // drops the row, which is all the swipe asked for.
            switch try store.entry(id: id, for: babyID) {
            case .diaper(let diaper)?: try store.softDelete(diaper)
            case .supplement(let med)?: try store.softDelete(med)
            default: break
            }
        case .entry(.immunization(let id)):
            if case .immunization(let shot)? = try store.entry(id: id, for: babyID) { try store.softDelete(shot) }
        case .entry(.pump(let id)):
            // Gone already (deleted on the other phone): the reload drops it.
            if let pump = try store.pump(id: id) { try store.softDelete(pump) }
        case .entry(.measurement(let id)):
            if let check = try store.measurements(for: babyID).first(where: { $0.id == id }) {
                try store.softDelete(check)
            }
        case .runningTimer:
            throw HistoryDeleteError.runningTimer
        }
        try load(dayCount: dayCount)
    }

    // MARK: - Rows

    /// The family's running pump, on today in every baby's History, as a
    /// running timer is. Its clock ticks in the view; it opens the pump panel.
    private func runningPumpRow(_ pump: PumpEntry) -> HistoryListRow {
        HistoryListRow(id: pump.id, tracker: .pump, start: Self.clockTime(pump.startedAt, zone: pump.timeZoneID),
                       end: L10n.tr("now"), chips: [], text: HistoryCopy.pumping, isRunning: true,
                       runningSince: pump.startedAt, target: .entry(.pump(pump.id)), deleteConfirmation: false)
    }

    /// The live duration ticks in the view; the row itself only changes when
    /// the timer does.
    private func runningRow(_ timer: ActiveTimer, builder: HistoryDayBuilder) -> HistoryListRow {
        HistoryListRow(
            id: timer.id,
            tracker: timer.kind == .feed ? .feed : .sleep,
            start: Self.clockTime(timer.startedAt, zone: timer.timeZoneID),
            end: L10n.tr("now"),
            chips: timer.completedComponents.enumerated().map { builder.chip($0.offset, $0.element) },
            text: nil,
            isRunning: true,
            runningSince: timer.kind == .feed
                ? timer.currentBottleStartedAt ?? timer.currentSideStartedAt
                : timer.startedAt,
            target: .runningTimer(timer.id),
            deleteConfirmation: false
        )
    }

    // MARK: - Formatting

    /// Whole minutes, as in the approved mockup ("12m", "1h 2m"); a part
    /// under a minute keeps its seconds ("45s") rather than reading "0m"
    /// (H-R19). The part editor still shows the exact length.
    static func chipDuration(_ seconds: Int) -> String {
        seconds < 60 ? ElapsedFormatter.precise(seconds) : ElapsedFormatter.total(seconds)
    }

    static func title(for day: DayKey, today: DayKey, yesterday: DayKey?, zone: String) -> String {
        if day == today { return L10n.tr("Today") }
        if day == yesterday { return L10n.tr("Yesterday") }
        let bounds = DayBucket.bounds(of: day, timeZoneID: zone)
        var style = Date.FormatStyle(date: .abbreviated, time: .omitted)
        style.timeZone = TimeZone(identifier: zone) ?? .gmt
        return bounds.start.formatted(style)
    }

    static func clockTime(_ date: Date, zone: String) -> String {
        var style = Date.FormatStyle(date: .omitted, time: .shortened)
        style.timeZone = TimeZone(identifier: zone) ?? .gmt
        return date.formatted(style)
    }
}
