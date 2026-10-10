import Foundation
import Observation
import BabyCore
import BabyStore

/// Every string the running-timer editor shows. Strings never live in a view
/// — see `HomeCopy` — and these are asserted in `RunningTimerEditTests`.
enum RunningTimerEditCopy {
    static let feedTitle = HomeL10n.text("Edit feed")
    static let sleepTitle = HomeL10n.text("Edit sleep")
    static let cancel = HomeL10n.text("Cancel")
    static let save = HomeL10n.text("Save")

    static let startLabel = HomeL10n.text("Started")
    /// The nudges beside the picker. Most corrections are "I tapped Start a
    /// few minutes late", and a one-handed parent should not have to drive a
    /// wheel for that.
    static let startEarlier = HomeL10n.text("5 minutes earlier")
    static let startLater = HomeL10n.text("5 minutes later")
    static let startEarlierShort = HomeL10n.text("−5 min")
    static let startLaterShort = HomeL10n.text("+5 min")

    static let soFarHeader = HomeL10n.text("So far")
    static let nowHeader = HomeL10n.text("Now")
    static let nowFooter = HomeL10n.text("The part that's running keeps counting. Stop it to finish.")
    static let contentsLabel = HomeL10n.text("Contents")
    /// A part's rows: when it began and ended, and the length that makes.
    static let partStart = HomeL10n.text("Start")
    static let partEnd = HomeL10n.text("End")
    static let running = HomeL10n.text("Running")
    static let length = HomeL10n.text("Length")

    /// On the dial, for VoiceOver: a tappable time is not otherwise obviously
    /// an editor.
    static let dialHint = HomeL10n.text("Edit the start time and the parts so far")

    static func breastPart(_ side: BreastSide) -> String { HomeL10n.breast(side) }
    /// Numbered only when there is more than one, so "Bottle" is not "Bottle 1"
    /// of one.
    static func bottlePart(_ number: Int, of count: Int) -> String {
        count > 1 ? HomeL10n.format("Bottle %d", number) : HomeL10n.text("Bottle")
    }
    static func volumeAccessibilityLabel(_ part: String) -> String { HomeL10n.format("%@ volume", part) }
    static func contentsAccessibilityLabel(_ part: String) -> String { HomeL10n.format("%@ contents", part) }
    /// A finished part's Start and End pickers are otherwise announced only
    /// as "Start"/"End", with no word of which part that is.
    static func partStartAccessibilityLabel(_ part: String) -> String { HomeL10n.format("%@ start", part) }
    static func partEndAccessibilityLabel(_ part: String) -> String { HomeL10n.format("%@ end", part) }

    static let startInFuture = HomeL10n.text("The start can't be in the future.")
    /// `TimerEditError.startInFuture` also covers a start more than a week
    /// back. Calling that "the future" would be nonsense, so it gets neutral
    /// copy.
    static let startOutOfRange = HomeL10n.text("That start time is outside the allowed range.")
    static func startAfterCurrentSide(_ side: BreastSide, at time: String) -> String {
        HomeL10n.format(
            side == .left
                ? "The start can't be after the left breast began, at %@."
                : "The start can't be after the right breast began, at %@.",
            time
        )
    }
    static func startAfterCurrentBottle(at time: String) -> String {
        HomeL10n.format("The start can't be after the bottle began, at %@.", time)
    }
    static func startBeforePreviousPart(at time: String) -> String {
        HomeL10n.format("The start can't be before the previous part ended, at %@.", time)
    }
    static let partsExceedElapsed = HomeL10n.text("The finished parts add up to more time than the feed has run. Shorten a part or move the start earlier.")
    static let timerChanged = HomeL10n.text("This timer changed on another phone. Close and open it again to edit it.")
    static func timerEnded(_ subject: String) -> String {
        HomeL10n.format("This timer was stopped on another phone — edit the %@ in History.", subject)
    }
}

/// What the running-timer editor renders, and the one write it makes.
///
/// Holds a value copy of the timer, not the `@Model`: the other phone can
/// stop the timer while this is open, and a deleted model read from a view
/// is a crash. Whether an edit would save is answered by `RunningTimerEdit`,
/// the same value the store judges with, so the editor cannot offer a Save
/// the store would refuse on its own terms.
@MainActor
@Observable
public final class RunningTimerEditModel: Identifiable {
    /// One finished part of the feed, as its row edits it. The kind and side
    /// are fixed: the spec edits values, never what a part is.
    public struct Part: Identifiable, Equatable, Sendable {
        public enum Kind: Equatable, Sendable {
            case breast(BreastSide)
            case bottle
        }

        public let id: UUID
        public let kind: Kind
        public let title: String
        /// Nil on an old part recorded without its times; it is shown with
        /// its length only and sent back unchanged.
        public var start: Date?
        public var end: Date?
        public var volumeML: Double
        public var contents: BottleContents
        /// The length recorded before any edit, for a part without times —
        /// and for one whose times are untouched, so a stored length the
        /// bounds would round differently is not sent as a change.
        let storedSeconds: Int?
        private let storedStart: Date?
        private let storedEnd: Date?

        init(id: UUID, kind: Kind, title: String, start: Date?, end: Date?,
             volumeML: Double, contents: BottleContents, storedSeconds: Int?) {
            self.id = id
            self.kind = kind
            self.title = title
            self.start = start
            self.end = end
            self.volumeML = volumeML
            self.contents = contents
            self.storedSeconds = storedSeconds
            self.storedStart = start
            self.storedEnd = end
        }

        /// Whole seconds between Start and End, as `RunningTimerEdit` counts
        /// them (it refuses a length that is not `end − start`).
        public var seconds: Int? {
            guard let start, let end else { return storedSeconds }
            if start == storedStart, end == storedEnd, let storedSeconds { return storedSeconds }
            return max(0, Int(end.timeIntervalSince(start)))
        }

        var component: FeedComponent {
            switch kind {
            case .breast(let side):
                .breast(id: id, side: side, seconds: seconds ?? 0, startedAt: start, endedAt: end)
            case .bottle:
                .bottle(id: id, volumeML: volumeML, contents: contents,
                        seconds: seconds.flatMap { $0 > 0 ? $0 : nil }, startedAt: start, endedAt: end)
            }
        }
    }

    /// The timer's id — one editor per timer, which is what `.sheet(item:)`
    /// keys on.
    public nonisolated let id: UUID
    public let title: String
    /// The part still running: its title and when it began. Read-only. A
    /// side that began with the timer moves with the start being edited, so
    /// its Start follows the picker.
    public var runningPart: (title: String, start: Date)? {
        guard let currentSide, let currentSideStartedAt else { return nil }
        return (RunningTimerEditCopy.breastPart(currentSide), sideMovesWithStart ? startedAt : currentSideStartedAt)
    }

    /// The running part's Start, date and time in the timer's zone — a side
    /// begun before midnight is on the other day.
    public var runningStartText: String? {
        runningPart.map {
            var style = Date.FormatStyle(date: .abbreviated, time: .shortened)
            style.timeZone = TimeZone(identifier: timeZoneID) ?? .gmt
            return $0.start.formatted(style)
        }
    }

    /// When whatever still runs began — the side or the bottle, whichever is
    /// earlier — following the start being edited when it began with the
    /// timer. Nil when nothing runs.
    private var runningSince: Date? {
        let side = currentSideStartedAt.map { sideMovesWithStart ? startedAt : $0 }
        let bottle = currentBottleStartedAt.map {
            RunningTimerEdit.sideMovesWithStart(startedAt: originalStart, currentSideStartedAt: $0) ? startedAt : $0
        }
        return [side, bottle].compactMap { $0 }.min()
    }

    /// The latest a finished part's Start or End may be: when the running
    /// part began, or now when nothing runs.
    public var latestPartEnd: Date { min(runningSince ?? store.now, store.now) }

    /// A finished part ending inside the running one. `RunningTimerEdit`
    /// only sums lengths, so a part moved later without growing passes it.
    private var partEndsPastRunning: Bool {
        guard let runningSince else { return false }
        return parts.contains { part in part.end.map { $0 > runningSince } ?? false }
    }
    /// What the picker offers: back to a day ago (or the current start, if
    /// that is further) — but never before the previous part of the session
    /// it joined ended — forward to now, or to when the running side began,
    /// when it began after the timer did.
    public let startRange: ClosedRange<Date>

    public var startedAt: Date { didSet { refusal = nil; refusedBeforePreviousPart = false } }
    public var parts: [Part] { didSet { refusal = nil; refusedBeforePreviousPart = false } }

    /// Set once the timer is found gone. Nothing can be saved after that.
    public private(set) var endedMessage: String?
    /// The store's answer when it disagreed with this copy — the other phone
    /// changed the timer after the editor opened.
    private var refusal: TimerEditError?
    /// When the part before this one, in the session it joined, ended. Re-read
    /// when the store refuses a start before it: the other phone may have
    /// moved it since the editor opened.
    private var previousPartEnd: Date?
    /// The store refused the start as before the previous part's end.
    private var refusedBeforePreviousPart = false

    private let store: BabyStore
    let unit: VolumeUnit
    public let isSleep: Bool
    private let timeZoneID: String
    private let originalStart: Date
    /// Nil when no side runs.
    private let currentSide: BreastSide?
    private let currentSideStartedAt: Date?
    private let currentBottleStartedAt: Date?
    private let completedComponents: [FeedComponent]
    private let sideMovesWithStart: Bool

    public init(timer: ActiveTimer, store: BabyStore, unit: VolumeUnit = .millilitres) {
        let now = store.now
        self.id = timer.id
        self.store = store
        self.unit = unit
        self.isSleep = timer.kind == .sleep
        self.timeZoneID = timer.timeZoneID
        self.title = timer.kind == .sleep ? RunningTimerEditCopy.sleepTitle : RunningTimerEditCopy.feedTitle
        self.originalStart = timer.startedAt
        self.currentSide = timer.currentSide
        let sideStart = timer.currentSide == nil ? nil : timer.currentSideStartedAt
        self.currentSideStartedAt = sideStart
        self.currentBottleStartedAt = timer.currentBottleStartedAt
        self.completedComponents = timer.completedComponents
        self.startedAt = timer.startedAt

        let bottleCount = timer.completedComponents.filter { $0.bottleContents != nil }.count
        var bottleNumber = 0
        // Solids never ride on a timer; if one ever did it is left as it is
        // rather than offered a control this editor does not have.
        self.parts = timer.completedComponents.compactMap { component in
            switch component {
            case let .breast(id, side, seconds, start, end):
                return Part(
                    id: id, kind: .breast(side), title: RunningTimerEditCopy.breastPart(side),
                    start: start, end: end, volumeML: 0, contents: .formula, storedSeconds: seconds
                )
            case let .bottle(id, volumeML, contents, seconds, start, end):
                bottleNumber += 1
                return Part(
                    id: id, kind: .bottle,
                    title: RunningTimerEditCopy.bottlePart(bottleNumber, of: bottleCount),
                    start: start, end: end, volumeML: volumeML, contents: contents, storedSeconds: seconds
                )
            case .solids:
                return nil
            }
        }
        self.sideMovesWithStart = RunningTimerEdit.sideMovesWithStart(
            startedAt: timer.startedAt,
            currentSideStartedAt: sideStart
        )

        let upper = RunningTimerEdit.latestStart(
            startedAt: timer.startedAt, currentSideStartedAt: sideStart,
            currentBottleStartedAt: timer.currentBottleStartedAt, now: now
        )
        let previousEnd = (try? store.previousPartEnd(of: timer)) ?? nil
        self.previousPartEnd = previousEnd
        var lower = min(timer.startedAt, now.addingTimeInterval(-24 * 3600))
        if let previousEnd { lower = min(max(lower, previousEnd), timer.startedAt) }
        self.startRange = lower...upper
    }

    // MARK: - Judging

    /// The edit as the store would take it, or why it would not.
    private var evaluation: Result<RunningTimerEdit, TimerEditError> {
        do {
            let edit = try RunningTimerEdit(
                startedAt: originalStart,
                currentSideStartedAt: currentSideStartedAt,
                currentBottleStartedAt: currentBottleStartedAt,
                completedComponents: completedComponents,
                newStart: startedAt,
                // Every part: `RunningTimerEdit` keeps only those that changed.
                changedComponents: parts.map(\.component),
                now: store.now
            )
            if partEndsPastRunning { return .failure(.partsExceedElapsed) }
            return .success(edit)
        } catch let error as TimerEditError {
            return .failure(error)
        } catch {
            return .failure(.unknownComponent)
        }
    }

    /// The store's own rule, judged here so the editor can say it first.
    private var startsBeforePreviousPart: Bool {
        if refusedBeforePreviousPart { return true }
        guard let previousPartEnd, startedAt != originalStart else { return false }
        return startedAt < previousPartEnd
    }

    public var canSave: Bool {
        guard endedMessage == nil, refusal == nil, !startsBeforePreviousPart,
              case .success = evaluation else { return false }
        return true
    }

    /// Nil when the edit would save, or when nothing has changed yet — an
    /// editor that has just opened has nothing to complain about.
    public var validationMessage: String? {
        if let endedMessage { return endedMessage }
        if let refusal { return message(for: refusal) }
        if startsBeforePreviousPart {
            return RunningTimerEditCopy.startBeforePreviousPart(
                at: HomeCopy.clockTime(previousPartEnd ?? originalStart, zone: timeZoneID)
            )
        }
        guard case .failure(let error) = evaluation else { return nil }
        return message(for: error)
    }

    private func message(for error: TimerEditError) -> String? {
        switch error {
        case .nothingChanged:
            nil
        case .startInFuture:
            startedAt > store.now ? RunningTimerEditCopy.startInFuture : RunningTimerEditCopy.startOutOfRange
        case .startAfterCurrentSide:
            RunningTimerEditCopy.startAfterCurrentSide(
                currentSide ?? .left,
                at: HomeCopy.clockTime(currentSideStartedAt ?? originalStart, zone: timeZoneID)
            )
        case .startAfterCurrentBottle:
            RunningTimerEditCopy.startAfterCurrentBottle(
                at: HomeCopy.clockTime(currentBottleStartedAt ?? originalStart, zone: timeZoneID)
            )
        case .partsExceedElapsed:
            RunningTimerEditCopy.partsExceedElapsed
        case .unknownComponent:
            RunningTimerEditCopy.timerChanged
        case .timerEnded:
            endedCopy
        }
    }

    private var endedCopy: String {
        RunningTimerEditCopy.timerEnded(isSleep ? HomeL10n.text("sleep") : HomeL10n.text("feed"))
    }

    // MARK: - Editing

    /// Nudges the start, held inside what the picker offers.
    public func shiftStart(by seconds: TimeInterval) {
        let moved = startedAt.addingTimeInterval(seconds)
        startedAt = min(max(moved, startRange.lowerBound), startRange.upperBound)
    }

    /// Re-checks that the timer still runs. Called when the store changes
    /// under an open editor — a sync may have applied the other phone's stop.
    public func refreshAvailability() {
        guard endedMessage == nil else { return }
        if (try? store.activeTimer(id: id)) == nil { endedMessage = endedCopy }
    }

    /// Sends only what changed, then pushes the Live Activity — the store
    /// leaves that to the caller because the push is async. True when saved;
    /// false when the store refused, with `validationMessage` saying why.
    /// Throws only what is not a refusal (a failed write), for the banner.
    @discardableResult
    public func save() async throws -> Bool {
        guard endedMessage == nil, !startsBeforePreviousPart, case .success(let edit) = evaluation else { return false }
        do {
            try store.editRunningTimer(
                id: id,
                startedAt: edit.changedStart,
                components: edit.changedComponents
            )
        } catch TimerEditError.nothingChanged {
            // The other phone made this same correction while the sheet was
            // open. What the user asked for is what the timer now says, so
            // that is a save, not a dead end with Save greyed out.
        } catch TimerEditError.timerEnded {
            endedMessage = endedCopy
            return false
        } catch let error as TimerEditError {
            refusal = error
            return false
        } catch PartError.startBeforePreviousPart {
            if let timer = try? store.activeTimer(id: id), let end = try? store.previousPartEnd(of: timer) {
                previousPartEnd = end
            }
            refusedBeforePreviousPart = true
            return false
        }
        // `try?`: the edit has committed by now. A failed read here must not
        // surface as "couldn't save" — the Live Activity just waits for the
        // next push.
        if let timer = try? store.activeTimer(id: id) {
            await LiveActivityController.update(for: timer, in: store, own: true)
        }
        return true
    }

    // MARK: - Display

    /// A part's length, or nil for a bottle whose length was never known.
    public func lengthText(for part: Part) -> String? { part.seconds.map(ElapsedFormatter.precise) }

    public func volumeText(for part: Part) -> String {
        Volume(millilitres: part.volumeML).formatted(as: unit)
    }

    /// Initial stepper range. The sheet extends it for larger typed amounts.
    public static let volumeRange: ClosedRange<Double> = 0...400

    /// Derived from the display unit: 5 ml, or half an ounce.
    public var volumeStep: Double {
        unit == .millilitres ? 5 : Volume(ounces: 0.5).millilitres
    }

    public var cancelLabel: String { RunningTimerEditCopy.cancel }
    public var saveLabel: String { RunningTimerEditCopy.save }
}
