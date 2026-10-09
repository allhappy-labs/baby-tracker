import Foundation
import Observation
import BabyCore
import BabyStore

/// One part of a session, as History edits it: a breast or bottle component
/// of a feed entry, or a whole sleep (ruling H-R2).
public enum PartEditTarget: Hashable, Sendable {
    case breast(feedID: UUID, componentID: UUID)
    case bottle(feedID: UUID, componentID: UUID)
    case sleep(sleepID: UUID)

    /// The entry the part belongs to.
    public var entryID: UUID {
        switch self {
        case .breast(let feedID, _), .bottle(let feedID, _): feedID
        case .sleep(let sleepID): sleepID
        }
    }

    public var sessionKind: SessionKind {
        switch self {
        case .breast, .bottle: .feed
        case .sleep: .sleep
        }
    }
}

/// Every string the part editors show.
public enum PartEditCopy {
    public static let cancel = LoggingL10n.text("Cancel")
    public static let breastTitle = LoggingL10n.text("Breast")
    public static let bottleTitle = LoggingL10n.text("Bottle")
    public static let sideLabel = LoggingL10n.text("Side")
    public static let startLabel = LoggingL10n.text("Start")
    public static let endLabel = LoggingL10n.text("End")
    public static let durationLabel = LoggingL10n.text("Duration")
    public static let kindLabel = LoggingL10n.text("Kind")
    public static let nap = LoggingL10n.text("Nap")
    public static let night = LoggingL10n.text("Night")
    /// The sleep kind left to the classifier, which goes by the start time.
    public static let auto = LoggingL10n.text("Auto")
    public static let detailsLabel = LoggingL10n.text("Details")
    public static let noteLabel = LoggingL10n.text("Note")
    public static let notePrompt = LoggingL10n.text("Optional")

    /// Ruling H-R4's message, the same words Home's running-timer editor uses.
    public static func startBeforePreviousPart(at time: String) -> String {
        LoggingL10n.format("The start can't be before the previous part ended, at %@.", time)
    }

    /// Ruling H-R15's message: a start moved past the next part's start
    /// would reorder the session.
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
/// duration; a bottle's volume, contents and time; or a sleep's start, end
/// and kind — each with the entry's note.
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
    /// A breast part's length. Its end follows: moving the start keeps the
    /// duration, as the Past entry layout reads.
    public var durationSeconds: Int
    public var side: BreastSide
    public var volumeML: Double
    public var contents: BottleContents
    /// Nil is Auto: the classification derived from the start time.
    public var sleepKind: SleepKind?
    public var note: String
    /// A sleep's own end. A breast part's end is `start + durationSeconds`,
    /// and a bottle's moves with its time; see `end`.
    private var sleepEnd: Date
    /// True once the part has vanished, for example after the other phone
    /// deleted it. The sheet closes on it.
    public private(set) var isGone: Bool

    /// The part as last read, so `reload()` takes the other phone's change
    /// only into fields the user has not touched, and a start is checked
    /// against the previous part only once it moves (ruling H-R12).
    private var loaded: Draft
    /// Where a moved start may go: after the part before this one ended, and
    /// not after the part after it began — the store's answer for the other
    /// entries of the session, and the other components of an old combined
    /// entry. Read on load and reload rather than per render.
    private var bounds: Bounds
    /// The zone the entry was logged in, for the time the H-R4 and H-R15
    /// messages quote.
    private var zoneID: String

    private struct Bounds {
        var previousPartEnd: Date?
        var nextPartStart: Date?
    }

    private struct Draft: Equatable {
        var start: Date
        var durationSeconds: Int = 0
        var sleepEnd: Date
        /// False for a sleep stored with no end: saving leaves it open unless
        /// the end is edited.
        var hasEnd = true
        /// A timed bottle's length, kept when its time moves.
        var bottleLength: TimeInterval?
        var side: BreastSide = .left
        var volumeML: Double = 0
        var contents: BottleContents = .formula
        var sleepKind: SleepKind?
        var note: String = ""
    }

    public init(store: BabyStore, babyID: UUID, target: PartEditTarget, unit: VolumeUnit = .millilitres) {
        self.store = store
        self.babyID = babyID
        self.target = target
        self.unit = unit
        let read = Self.read(target, babyID: babyID, in: store)
        let draft = read?.draft ?? Draft(start: store.now, sleepEnd: store.now)
        loaded = draft
        start = draft.start
        durationSeconds = draft.durationSeconds
        sleepEnd = draft.sleepEnd
        side = draft.side
        volumeML = draft.volumeML
        contents = draft.contents
        sleepKind = draft.sleepKind
        note = draft.note
        bounds = read?.bounds ?? Bounds()
        zoneID = read?.zoneID ?? TimeZone.current.identifier
        isGone = read == nil
    }

    /// Breast: `start + durationSeconds`, and setting it sets the duration.
    /// Sleep: its own end. Bottle: its time, or a timed bottle's end.
    public var end: Date {
        get {
            switch target {
            case .breast: start.addingTimeInterval(TimeInterval(durationSeconds))
            case .bottle: start.addingTimeInterval(loaded.bottleLength ?? 0)
            case .sleep: sleepEnd
            }
        }
        set {
            switch target {
            case .breast: durationSeconds = max(0, Int(newValue.timeIntervalSince(start).rounded()))
            case .bottle: break
            case .sleep: sleepEnd = newValue
            }
        }
    }

    /// Now, or the part's own end if it is somehow already ahead of the
    /// clock, so opening such a part to fix its note stays saveable.
    public var latestAllowedDate: Date {
        max(store.now, loadedEnd)
    }

    /// The part's end as it was read.
    private var loadedEnd: Date {
        switch target {
        case .breast: loaded.start.addingTimeInterval(TimeInterval(loaded.durationSeconds))
        case .bottle: loaded.start.addingTimeInterval(loaded.bottleLength ?? 0)
        case .sleep: loaded.hasEnd ? loaded.sleepEnd : loaded.start
        }
    }

    /// Nil when the part can be saved as it stands. The same rules as the
    /// Past entry sheet, plus rulings H-R4 and H-R15: a start moved before
    /// the previous part ended, or after the next part began, says when that
    /// was; so does an end moved past the next part's start (final review
    /// M3). Like the start (ruling H-R12), the 30-second floor and the end
    /// are judged only once they change, so an old part stays editable.
    public var validationMessage: String? {
        guard !isGone else { return nil }
        let latest = latestAllowedDate
        switch target {
        case .breast:
            if durationSeconds != loaded.durationSeconds, durationSeconds < 30 {
                return PastEntrySheetCopy.breastTooShort
            }
            if end > latest { return PastEntrySheetCopy.inTheFuture }
        case .bottle:
            if start > latest { return PastEntrySheetCopy.inTheFuture }
        case .sleep:
            if writesSleepEnd, end <= start { return PastEntrySheetCopy.endBeforeStart }
            if start > latest || (writesSleepEnd && end > latest) { return PastEntrySheetCopy.inTheFuture }
        }
        if start != loaded.start {
            if let previousPartEnd = bounds.previousPartEnd, start < previousPartEnd {
                return PartEditCopy.startBeforePreviousPart(at: JoinRowModel.clockTime(previousPartEnd, zone: zoneID))
            }
            if let nextPartStart = bounds.nextPartStart, start > nextPartStart {
                return PartEditCopy.startAfterNextPart(at: JoinRowModel.clockTime(nextPartStart, zone: zoneID))
            }
        }
        if end != loadedEnd, let nextPartStart = bounds.nextPartStart, end > nextPartStart {
            return PartEditCopy.endAfterNextPart(at: JoinRowModel.clockTime(nextPartStart, zone: zoneID))
        }
        return nil
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
        case let .breast(feedID, componentID), let .bottle(feedID, componentID):
            guard let (feed, index) = locatedFeed(feedID, componentID) else {
                isGone = true
                throw EditModeError.gone
            }
            guard canSave else { return false }
            if start != loaded.start {
                guard try passesStoreCheck({ try store.checkPartStart(start, ofFeed: feedID, for: babyID) },
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
            if start != loaded.start {
                guard try passesStoreCheck({ try store.checkSleepPartStart(start, ofSleep: sleepID, for: babyID) },
                                           refresh: { Self.bounds(ofSleep: sleepID, babyID: babyID, in: store) })
                else { return false }
            }
            try write(sleep)
            loaded = Self.draft(sleep)
            bounds = Self.bounds(ofSleep: sleepID, babyID: babyID, in: store)
        }
        return true
    }

    /// Runs the store's part-start check. On its refusal the editor's own
    /// bounds were out of date: take the store's, so the message shows the
    /// time it refused on, and say the save may not go ahead.
    private func passesStoreCheck(_ check: () throws -> Void, refresh: () -> Bounds) throws -> Bool {
        do {
            try check()
            return true
        } catch PartError.startBeforePreviousPart, PartError.startAfterNextPart {
            bounds = refresh()
            return false
        }
    }

    /// Removes the part the session-aware way (rulings H-R2, H-R11): one
    /// component comes out of its entry, and the entry goes with its last
    /// one; a sleep is deleted with its session relinked. Never `softDelete`.
    public func delete() throws {
        switch target {
        case let .breast(feedID, componentID), let .bottle(feedID, componentID):
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
        EditModel.merge(&durationSeconds, loaded: loaded.durationSeconds, fresh: fresh.durationSeconds)
        EditModel.merge(&sleepEnd, loaded: loaded.sleepEnd, fresh: fresh.sleepEnd)
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
    private var writesSleepEnd: Bool { loaded.hasEnd || sleepEnd != loaded.sleepEnd }

    private func write(_ feed: FeedEntry, index: Int) throws {
        let before = (feed.components, feed.startedAt, feed.endedAt, feed.note, feed.lastModifiedAt)
        // The stored times change only as far as the user moved them: a side,
        // volume or note edit leaves an old part's times exactly as stored,
        // and a new duration alone never invents a start the part did not
        // record — except a lone part's, whose time is its feed's time. The
        // entry's span follows only a part whose times moved.
        let startMoved = start != loaded.start
        let durationChanged = durationSeconds != loaded.durationSeconds
        let refit: Bool
        switch feed.components[index] {
        case let .breast(id, _, seconds, startedAt, endedAt):
            let knownStart = startedAt ?? (feed.components.count == 1 ? feed.startedAt : nil)
            let newStart = startMoved ? start : knownStart
            let timesChanged = startMoved || durationChanged
            feed.components[index] = .breast(
                id: id, side: side, seconds: durationChanged ? durationSeconds : seconds,
                startedAt: timesChanged ? newStart : startedAt,
                endedAt: timesChanged ? newStart.map { $0.addingTimeInterval(TimeInterval(durationSeconds)) } : endedAt)
            refit = timesChanged && newStart != nil
        case let .bottle(id, _, _, seconds, startedAt, endedAt):
            let shift = start.timeIntervalSince(startedAt ?? loaded.start)
            feed.components[index] = .bottle(
                id: id, volumeML: volumeML, contents: contents, seconds: seconds,
                startedAt: startMoved ? start : startedAt,
                endedAt: startMoved ? endedAt.map { $0.addingTimeInterval(shift) } : endedAt)
            refit = startMoved
        case .solids:
            return
        }
        if refit { Self.fitSpan(of: feed, edited: index, previousStart: before.1) }
        feed.note = EditModel.trimmedToNil(note)
        do {
            try store.update(feed)
        } catch {
            (feed.components, feed.startedAt, feed.endedAt, feed.note, feed.lastModifiedAt) = before
            throw error
        }
    }

    private func write(_ sleep: SleepEntry) throws {
        let before = (sleep.startedAt, sleep.endedAt, sleep.kindOverrideRaw, sleep.note, sleep.lastModifiedAt)
        sleep.startedAt = start
        if writesSleepEnd { sleep.endedAt = sleepEnd }
        sleep.kindOverride = sleepKind
        sleep.note = EditModel.trimmedToNil(note)
        do {
            try store.update(sleep)
        } catch {
            (sleep.startedAt, sleep.endedAt, sleep.kindOverrideRaw, sleep.note, sleep.lastModifiedAt) = before
            throw error
        }
    }

    /// The entry's span follows its parts. A one-part entry's time is the
    /// part's time. Several parts all knowing their starts set the span
    /// exactly; an old part with no start leaves it to widen only, since that
    /// part's place is unknown.
    private static func fitSpan(of feed: FeedEntry, edited index: Int, previousStart: Date) {
        let timed = feed.components.filter { $0.breastSide != nil || $0.bottleContents != nil }
        if feed.components.count == 1, let start = feed.components[0].startedAt {
            let shift = start.timeIntervalSince(previousStart)
            feed.startedAt = start
            feed.endedAt = feed.components[0].endedAt ?? feed.endedAt.map { $0.addingTimeInterval(shift) }
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
        var draft = Draft(start: start, sleepEnd: start, note: feed.note ?? "")
        switch component {
        case let .breast(_, side, seconds, _, _):
            draft.side = side
            draft.durationSeconds = seconds
        case let .bottle(_, volumeML, contents, _, startedAt, endedAt):
            draft.volumeML = volumeML
            draft.contents = contents
            if let startedAt, let endedAt { draft.bottleLength = endedAt.timeIntervalSince(startedAt) }
        case .solids:
            break
        }
        return draft
    }

    private static func draft(_ sleep: SleepEntry) -> Draft {
        Draft(start: sleep.startedAt, sleepEnd: sleep.endedAt ?? sleep.startedAt, hasEnd: sleep.endedAt != nil,
              sleepKind: sleep.kindOverride, note: sleep.note ?? "")
    }

    /// A feed part's bounds: the store's (the entries before and after this
    /// one in its session), narrowed by the components before and after this
    /// one in its own entry, for an old combined feed.
    private static func bounds(of feed: FeedEntry, index: Int, babyID: UUID, in store: BabyStore) -> Bounds {
        let previous = try? store.previousPartEnd(ofFeed: feed.id, for: babyID)
        let next = try? store.nextPartStart(ofFeed: feed.id, for: babyID)
        let ownEarlier = feed.components[..<index].compactMap { $0.endedAt ?? $0.startedAt }.max()
        let ownLater = feed.components[(index + 1)...].compactMap(\.startedAt).min()
        return Bounds(previousPartEnd: [previous, ownEarlier].compactMap { $0 }.max(),
                      nextPartStart: [next, ownLater].compactMap { $0 }.min())
    }

    /// A sleep part's bounds: the sleeps before and after it in its session.
    private static func bounds(ofSleep sleepID: UUID, babyID: UUID, in store: BabyStore) -> Bounds {
        Bounds(previousPartEnd: try? store.previousPartEnd(ofSleep: sleepID, for: babyID),
               nextPartStart: try? store.nextPartStart(ofSleep: sleepID, for: babyID))
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
