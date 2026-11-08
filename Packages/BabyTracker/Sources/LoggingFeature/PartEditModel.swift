import Foundation
import Observation
import BabyCore
import BabyStore

/// The kind of part an editor shows, so the view picks its rows.
public enum PartKind: Hashable, CaseIterable, Sendable {
    case breast, bottle, sleep
}

/// One part of a session, as History edits it: a breast or bottle component
/// of a feed entry, or a whole sleep (ruling H-R2) — or solids eaten during
/// a feed, which the Solids panel edits rather than `PartEditModel`.
public enum PartEditTarget: Hashable, Sendable, Identifiable {
    public var id: Self { self }

    case breast(feedID: UUID, componentID: UUID)
    case bottle(feedID: UUID, componentID: UUID)
    case solids(feedID: UUID, componentID: UUID)
    case sleep(sleepID: UUID)

    /// The entry the part belongs to.
    public var entryID: UUID {
        switch self {
        case .breast(let feedID, _), .bottle(let feedID, _), .solids(let feedID, _): feedID
        case .sleep(let sleepID): sleepID
        }
    }

    public var sessionKind: SessionKind {
        switch self {
        case .breast, .bottle, .solids: .feed
        case .sleep: .sleep
        }
    }
}

/// Every string the part editors show.
public enum PartEditCopy {
    public static let breastTitle = LoggingL10n.text("Breast")
    public static let bottleTitle = LoggingL10n.text("Bottle")
    /// New mode's title: keeps the More sheet's wording.
    public static let newTitle = LoggingL10n.text("Past entry")
    public static let sideLabel = LoggingL10n.text("Side")
    public static let kindLabel = LoggingL10n.text("Kind")
    public static let nap = LoggingL10n.text("Nap")
    public static let night = LoggingL10n.text("Night")
    /// The sleep kind left to the classifier, which goes by the start time.
    public static let auto = LoggingL10n.text("Auto")

    /// Ruling H-R4's message, the same words Home's running-timer editor uses.
    public static func startBeforePreviousPart(at time: String) -> String {
        LoggingL10n.format("The start can't be before the previous part ended, at %@.", time)
    }

    /// A part of an old combined feed keeps its place among its entry's
    /// other parts, so its start may not pass the next one's.
    public static func startAfterNextPart(at time: String) -> String {
        LoggingL10n.format("The start can't be after the next part began, at %@.", time)
    }

    /// A longer part may not run into the next one, which would count the
    /// overlap twice (final review M3).
    public static func endAfterNextPart(at time: String) -> String {
        LoggingL10n.format("The end can't be after the next part began, at %@.", time)
    }
}

/// The state and writes of one part's editor: a breast side's side, start and
/// end; a bottle's volume, contents, start and optional end; or a sleep's
/// start, end and kind — each with the entry's note. Start and End are
/// independent: moving one never moves the other.
///
/// Holds ids, not models. The other phone may change or delete the part while
/// the editor is open, so every write fetches it afresh, and `reload()`, which
/// the sheet calls on every `store.revision`, notices when it has gone.
@MainActor @Observable
public final class PartEditModel {
    let store: BabyStore
    let babyID: UUID
    public let target: PartEditTarget
    public let unit: VolumeUnit

    public var start: Date
    /// Breast and sleep: always set. Bottle: nil for a bottle given at a moment.
    public var end: Date?
    public var side: BreastSide
    public var volumeML: Double
    public var contents: BottleContents
    /// Nil is Auto: the classification derived from the start time.
    public var sleepKind: SleepKind?
    public var note: String
    /// True once the part has vanished, for example after the other phone
    /// deleted it. The sheet closes on it.
    public private(set) var isGone: Bool

    /// The part as last read, so `reload()` takes the other phone's change
    /// only into fields the user has not touched, and a start is checked
    /// against the previous part only once it moves (ruling H-R12).
    private var loaded: Draft
    /// Where moved times may go: anywhere the session's other entries leave
    /// free, before or after any of them — the store's answer — and, for a
    /// part of an old combined entry, between that entry's parts before and
    /// after it. Read on load and reload rather than per render.
    private var bounds: Bounds
    /// The zone the entry was logged in, for the time the H-R4 and H-R15
    /// messages quote.
    private var zoneID: String

    private struct Bounds {
        /// The other entries of the session.
        var others: [PartSpan] = []
        /// The end of the part before, and the start of the part after, in
        /// this part's own old combined entry.
        var previousPartEnd: Date?
        var nextPartStart: Date?
    }

    private struct Draft: Equatable {
        var start: Date
        var end: Date?
        /// False for a sleep stored with no end: saving leaves it open unless
        /// the end is edited.
        var hasEnd = true
        var side: BreastSide = .left
        var volumeML: Double = 0
        var contents: BottleContents = .formula
        var sleepKind: SleepKind?
        var note: String = ""

        var seconds: Int? { TimeSpan(start: start, end: end).seconds }
    }

    public init(store: BabyStore, babyID: UUID, target: PartEditTarget, unit: VolumeUnit = .millilitres) {
        self.store = store
        self.babyID = babyID
        self.target = target
        self.unit = unit
        let read = Self.read(target, babyID: babyID, in: store)
        let draft = read?.draft ?? Draft(start: store.now, end: store.now)
        loaded = draft
        start = draft.start
        end = draft.end
        side = draft.side
        volumeML = draft.volumeML
        contents = draft.contents
        sleepKind = draft.sleepKind
        note = draft.note
        bounds = read?.bounds ?? Bounds()
        zoneID = read?.zoneID ?? TimeZone.current.identifier
        isGone = read == nil
    }

    /// The length the rows show, never edited directly.
    public var durationSeconds: Int { TimeSpan(start: start, end: end).seconds ?? 0 }

    public var kind: PartKind {
        switch target {
        case .breast: .breast
        // Solids open the Solids panel instead; this editor finds nothing
        // for them and is gone at once.
        case .bottle, .solids: .bottle
        case .sleep: .sleep
        }
    }

    /// Now, or the part's own end if it is somehow already ahead of the
    /// clock, so opening such a part to fix its note stays saveable.
    public var latestAllowedDate: Date {
        max(store.now, loadedEnd ?? loaded.start)
    }

    /// The part's end as it was read; nil for a moment's bottle or an open
    /// sleep.
    private var loadedEnd: Date? {
        switch target {
        case .breast, .bottle, .solids: loaded.end
        case .sleep: loaded.hasEnd ? loaded.end : nil
        }
    }

    /// Nil when the part can be saved as it stands. The same rules as the
    /// Past entry sheet, plus ruling H-R4 as overlap: moved times may not run
    /// into another part of the session, and the message says where that
    /// part ends or begins. They may pass one, which reorders the session.
    /// Like the start (ruling H-R12), the 30-second floor and the overlap
    /// are judged only once times change, so an old part stays editable.
    public var validationMessage: String? {
        guard !isGone else { return nil }
        let latest = latestAllowedDate
        let timesMoved = start != loaded.start || end != loaded.end
        switch target {
        case .breast:
            // The future and an end moved before its start are judged
            // whenever a time moved; the 30-second floor only once the
            // length itself changes, so an old short part keeps its side and
            // note edits. Without the `endBeforeStart` case here, a legacy
            // 0-second part could have its Start dragged past its End: the
            // clamped length (`durationSeconds`, floored at 0) would still
            // read 0, same as `loaded.seconds`, and the reversal would slip
            // through unflagged.
            if timesMoved, let problem = TimeSpan(start: start, end: end).problem(latest: latest, minimumSeconds: 30),
               problem == .inTheFuture || problem == .endBeforeStart || durationSeconds != loaded.seconds {
                return TimeSpanCopy.message(problem)
            }
        case .bottle, .solids:
            if timesMoved, let problem = TimeSpan(start: start, end: end).problem(latest: latest) {
                return TimeSpanCopy.message(problem)
            }
        case .sleep:
            if timesMoved, let problem = TimeSpan(start: start, end: writesSleepEnd ? end : nil).problem(latest: latest) {
                return TimeSpanCopy.message(problem)
            }
        }
        if start != loaded.start {
            if let previousPartEnd = bounds.previousPartEnd, start < previousPartEnd {
                return PartEditCopy.startBeforePreviousPart(at: JoinRowModel.clockTime(previousPartEnd, zone: zoneID))
            }
            if let nextPartStart = bounds.nextPartStart, start > nextPartStart {
                return PartEditCopy.startAfterNextPart(at: JoinRowModel.clockTime(nextPartStart, zone: zoneID))
            }
        }
        if end != loadedEnd, let end, let nextPartStart = bounds.nextPartStart, end > nextPartStart {
            return PartEditCopy.endAfterNextPart(at: JoinRowModel.clockTime(nextPartStart, zone: zoneID))
        }
        if timesMoved, let clash = PartSpan.clash(span, was: loadedSpan, among: bounds.others) {
            return clash.start <= start
                ? PartEditCopy.startBeforePreviousPart(at: JoinRowModel.clockTime(clash.end, zone: zoneID))
                : PartEditCopy.endAfterNextPart(at: JoinRowModel.clockTime(clash.start, zone: zoneID))
        }
        return nil
    }

    /// The part's span as edited, and as it was read: an open sleep's, or a
    /// moment's, is its start.
    private var span: PartSpan { PartSpan(start: start, end: writesTimedEnd ? end ?? start : start) }
    private var loadedSpan: PartSpan { PartSpan(start: loaded.start, end: loadedEnd ?? loaded.start) }
    private var writesTimedEnd: Bool {
        if case .sleep = target { return writesSleepEnd }
        return true
    }

    /// A zero-volume bottle renders while every total that gates on `> 0`
    /// leaves it out, so a change *to* 0 is refused, as on the Bottle sheet.
    /// A legacy bottle already stored at 0 stays editable — its note or time
    /// — as long as its volume is left alone (ruling H-R24, like H-R12).
    public var canSave: Bool {
        guard !isGone, validationMessage == nil else { return false }
        if case .bottle = target { return volumeML > 0 || volumeML == loaded.volumeML }
        return true
    }

    /// Writes the part in one `.update`. Returns false, writing nothing, when
    /// the draft is refused: `validationMessage` then says why. A start that
    /// moved is put to the store's own check first (rulings H-R4, H-R14,
    /// H-R15), and only then (ruling H-R12). A refused write puts the entry
    /// back as it was, so the edit never sits unsaved in the context for an
    /// unrelated save to commit without its change-log entry.
    public func save() throws -> Bool {
        switch target {
        case .solids:
            isGone = true
            throw EditModeError.gone
        case let .breast(feedID, componentID), let .bottle(feedID, componentID):
            guard let (feed, index) = locatedFeed(feedID, componentID) else {
                isGone = true
                throw EditModeError.gone
            }
            guard canSave else { return false }
            if start != loaded.start || end != loaded.end {
                guard try passesStoreCheck({ try store.checkPartSpan(span, was: loadedSpan, ofFeed: feedID, for: babyID) },
                                           refresh: { Self.bounds(of: feed, index: index, babyID: babyID, in: store) })
                else { return false }
            }
            try write(feed, index: index)
            loaded = Self.draft(feed, index: index)
            bounds = Self.bounds(of: feed, index: index, babyID: babyID, in: store)
        case let .sleep(sleepID):
            guard let sleep = Self.fetchSleep(sleepID, babyID: babyID, in: store) else {
                isGone = true
                throw EditModeError.gone
            }
            guard canSave else { return false }
            if start != loaded.start || end != loaded.end {
                guard try passesStoreCheck({ try store.checkSleepPartSpan(span, was: loadedSpan, ofSleep: sleepID,
                                                                          for: babyID) },
                                           refresh: { Self.bounds(ofSleep: sleepID, babyID: babyID, in: store) })
                else { return false }
            }
            try write(sleep)
            loaded = Self.draft(sleep)
            bounds = Self.bounds(ofSleep: sleepID, babyID: babyID, in: store)
        }
        return true
    }

    /// Runs the store's overlap check. On its refusal the editor's own
    /// bounds were out of date: take the store's, so the message shows the
    /// time it refused on, and say the save may not go ahead.
    private func passesStoreCheck(_ check: () throws -> Void, refresh: () -> Bounds) throws -> Bool {
        do {
            try check()
            return true
        } catch PartError.overlapsPart {
            bounds = refresh()
            return false
        }
    }

    /// Removes the part the session-aware way (rulings H-R2, H-R11): one
    /// component comes out of its entry, and the entry goes with its last
    /// one; a sleep is deleted with its session relinked. Never `softDelete`.
    public func delete() throws {
        switch target {
        case let .breast(feedID, componentID), let .bottle(feedID, componentID), let .solids(feedID, componentID):
            guard locatedFeed(feedID, componentID) != nil else {
                isGone = true
                throw EditModeError.gone
            }
            try store.removeComponent(componentID, fromFeed: feedID, for: babyID)
        case let .sleep(sleepID):
            guard Self.fetchSleep(sleepID, babyID: babyID, in: store) != nil else {
                isGone = true
                throw EditModeError.gone
            }
            try store.deleteSleepPart(sleepID, for: babyID)
        }
    }

    /// Re-reads the part after any write, this phone's or the other's.
    public func reload() {
        guard let read = Self.read(target, babyID: babyID, in: store) else {
            isGone = true
            return
        }
        let fresh = read.draft
        EditModel.merge(&start, loaded: loaded.start, fresh: fresh.start)
        EditModel.merge(&end, loaded: loaded.end, fresh: fresh.end)
        EditModel.merge(&side, loaded: loaded.side, fresh: fresh.side)
        EditModel.merge(&volumeML, loaded: loaded.volumeML, fresh: fresh.volumeML)
        EditModel.merge(&contents, loaded: loaded.contents, fresh: fresh.contents)
        EditModel.merge(&sleepKind, loaded: loaded.sleepKind, fresh: fresh.sleepKind)
        EditModel.merge(&note, loaded: loaded.note, fresh: fresh.note)
        loaded = fresh
        bounds = read.bounds
        zoneID = read.zoneID
    }

    // MARK: - Bottle amounts

    /// The Bottle sheet's four amounts in this unit.
    public var chips: [Double] { BottleSheetModel.chips(for: unit) }
    public var stepAmount: Double { BottleSheetModel.stepAmount(for: unit) }
    public func label(for ml: Double) -> String { BottleSheetModel.label(for: ml, unit: unit) }
    public func step(_ direction: Int) { volumeML = max(0, volumeML + Double(direction) * stepAmount) }

    // MARK: - Private

    /// A sleep stored with no end stays open unless its end is edited.
    private var writesSleepEnd: Bool { loaded.hasEnd || end != loaded.end }

    private func write(_ feed: FeedEntry, index: Int) throws {
        let before = (feed.components, feed.startedAt, feed.endedAt, feed.note, feed.lastModifiedAt)
        // The stored times change only as far as the user moved them: a side,
        // volume or note edit leaves an old part's times exactly as stored,
        // and an End edit alone never invents a start the part did not
        // record — except a lone part's, whose time is its feed's time. The
        // entry's span follows only a part whose times moved.
        let startMoved = start != loaded.start
        let timesChanged = startMoved || end != loaded.end
        let refit: Bool
        switch feed.components[index] {
        case let .breast(id, _, seconds, startedAt, endedAt):
            // An old part with no recorded start keeps it unknown unless the start
            // itself moved; an End edit changes only its length.
            let knownStart = startedAt ?? (feed.components.count == 1 ? feed.startedAt : nil)
            let newStart = startMoved ? start : knownStart
            let length = durationSeconds
            feed.components[index] = .breast(
                id: id, side: side, seconds: timesChanged ? length : seconds,
                startedAt: timesChanged ? newStart : startedAt,
                endedAt: timesChanged ? newStart.map { $0.addingTimeInterval(TimeInterval(length)) } : endedAt)
            refit = timesChanged && newStart != nil
        case let .bottle(id, _, _, seconds, startedAt, endedAt):
            // As for a breast part: an old bottle with no recorded time keeps
            // it unknown unless its start itself moved.
            let knownStart = startedAt ?? (feed.components.count == 1 ? feed.startedAt : nil)
            let newStart = startMoved ? start : knownStart
            let length = TimeSpan(start: start, end: end).seconds
            feed.components[index] = .bottle(
                id: id, volumeML: volumeML, contents: contents,
                seconds: timesChanged ? length : seconds,
                startedAt: timesChanged ? newStart : startedAt,
                endedAt: timesChanged ? newStart.flatMap { start in length.map { start.addingTimeInterval(TimeInterval($0)) } }
                                      : endedAt)
            refit = timesChanged && newStart != nil
        case .solids:
            return
        }
        if refit { Self.fitSpan(of: feed, edited: index) }
        feed.note = EditModel.trimmedToNil(note)
        do {
            try store.updateFeedPart(feed, for: babyID)
        } catch {
            (feed.components, feed.startedAt, feed.endedAt, feed.note, feed.lastModifiedAt) = before
            throw error
        }
    }

    private func write(_ sleep: SleepEntry) throws {
        let before = (sleep.startedAt, sleep.endedAt, sleep.kindOverrideRaw, sleep.note, sleep.lastModifiedAt)
        sleep.startedAt = start
        if writesSleepEnd { sleep.endedAt = end }
        sleep.kindOverride = sleepKind
        sleep.note = EditModel.trimmedToNil(note)
        do {
            try store.updateSleepPart(sleep, for: babyID)
        } catch {
            (sleep.startedAt, sleep.endedAt, sleep.kindOverrideRaw, sleep.note, sleep.lastModifiedAt) = before
            throw error
        }
    }

    /// The entry's span follows its parts. A one-part entry's time is the
    /// part's time. Several parts all knowing their starts set the span
    /// exactly; an old part with no start leaves it to widen only, since that
    /// part's place is unknown.
    private static func fitSpan(of feed: FeedEntry, edited index: Int) {
        let timed = feed.components.filter { $0.breastSide != nil || $0.bottleContents != nil }
        if feed.components.count == 1, let start = feed.components[0].startedAt {
            // A moment's entry ends where it starts.
            feed.startedAt = start
            feed.endedAt = feed.components[0].endedAt ?? start
        } else if !timed.isEmpty, timed.allSatisfy({ $0.startedAt != nil }),
                  let start = timed.compactMap(\.startedAt).min(),
                  let end = timed.compactMap({ $0.endedAt ?? $0.startedAt }).max() {
            feed.startedAt = start
            feed.endedAt = end
        } else if let start = feed.components[index].startedAt {
            let end = feed.components[index].endedAt ?? start
            feed.startedAt = min(feed.startedAt, start)
            feed.endedAt = max(feed.endedAt ?? end, end)
        }
    }

    private func locatedFeed(_ feedID: UUID, _ componentID: UUID) -> (FeedEntry, Int)? {
        Self.locatedFeed(feedID, componentID, babyID: babyID, in: store)
    }

    private static func read(_ target: PartEditTarget, babyID: UUID, in store: BabyStore)
        -> (draft: Draft, bounds: Bounds, zoneID: String)? {
        switch target {
        case .solids:
            return nil
        case let .breast(feedID, componentID), let .bottle(feedID, componentID):
            guard let (feed, index) = locatedFeed(feedID, componentID, babyID: babyID, in: store) else { return nil }
            let component = feed.components[index]
            switch (target, component) {
            case (.breast, .breast), (.bottle, .bottle): break
            default: return nil
            }
            return (draft(feed, index: index), bounds(of: feed, index: index, babyID: babyID, in: store),
                    feed.timeZoneID)
        case let .sleep(sleepID):
            guard let sleep = fetchSleep(sleepID, babyID: babyID, in: store) else { return nil }
            return (draft(sleep), bounds(ofSleep: sleepID, babyID: babyID, in: store), sleep.timeZoneID)
        }
    }

    private static func draft(_ feed: FeedEntry, index: Int) -> Draft {
        let component = feed.components[index]
        // A single-part feed's time is the feed's time.
        let start = feed.components.count == 1 ? feed.startedAt : component.startedAt ?? feed.startedAt
        var draft = Draft(start: start, end: nil, note: feed.note ?? "")
        switch component {
        case let .breast(_, side, seconds, _, _):
            draft.side = side
            draft.end = start.addingTimeInterval(TimeInterval(seconds))
        case let .bottle(_, volumeML, contents, seconds, startedAt, endedAt):
            draft.volumeML = volumeML
            draft.contents = contents
            if let startedAt, let endedAt, endedAt > startedAt {
                draft.end = start.addingTimeInterval(endedAt.timeIntervalSince(startedAt))
            } else if let seconds, seconds > 0 {
                // A length recorded without its times still opens as an end.
                draft.end = start.addingTimeInterval(TimeInterval(seconds))
            }
        case .solids:
            break
        }
        return draft
    }

    private static func draft(_ sleep: SleepEntry) -> Draft {
        Draft(start: sleep.startedAt, end: sleep.endedAt ?? sleep.startedAt, hasEnd: sleep.endedAt != nil,
              sleepKind: sleep.kindOverride, note: sleep.note ?? "")
    }

    /// A feed part's bounds: the store's (the other entries of its
    /// session), and the components before and after this one in its own
    /// entry, for an old combined feed.
    private static func bounds(of feed: FeedEntry, index: Int, babyID: UUID, in store: BabyStore) -> Bounds {
        Bounds(others: (try? store.otherPartSpans(ofFeed: feed.id, for: babyID)) ?? [],
               previousPartEnd: feed.components[..<index].compactMap { $0.endedAt ?? $0.startedAt }.max(),
               nextPartStart: feed.components[(index + 1)...].compactMap(\.startedAt).min())
    }

    /// A sleep part's bounds: the other sleeps of its session.
    private static func bounds(ofSleep sleepID: UUID, babyID: UUID, in store: BabyStore) -> Bounds {
        Bounds(others: (try? store.otherPartSpans(ofSleep: sleepID, for: babyID)) ?? [])
    }

    private static func locatedFeed(_ feedID: UUID, _ componentID: UUID, babyID: UUID,
                                    in store: BabyStore) -> (FeedEntry, Int)? {
        guard case .feed(let feed)? = try? store.entry(id: feedID, for: babyID),
              feed.realmKey == store.activeRealm.encodedKey,
              let index = feed.components.firstIndex(where: { $0.id == componentID }) else { return nil }
        return (feed, index)
    }

    /// Only from the active realm: after a switch to or from a family, an
    /// entry of the other realm is gone as far as this editor is concerned.
    private static func fetchSleep(_ id: UUID, babyID: UUID, in store: BabyStore) -> SleepEntry? {
        guard case .sleep(let sleep)? = try? store.entry(id: id, for: babyID),
              sleep.realmKey == store.activeRealm.encodedKey else { return nil }
        return sleep
    }
}
