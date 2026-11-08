import SwiftUI
import BabyCore
import BabyStore
import BabyUI

/// What this sheet is open to do.
public enum BottleSheetMode: Sendable, Equatable {
    /// Start a timed bottle, or log what was drunk right now.
    case start
    /// A running bottle just stopped and needs its amount. `done()` finishes
    /// the bottle and ends the feed in one write.
    case stopped(timerID: UUID)
    /// A bottle already logged needs its amount set after the fact.
    case amount(feedID: UUID)
    /// Edits one bottle already logged, alone or a part of a session: its
    /// volume, contents and time, with Save and Delete. Only that component
    /// changes; a single-bottle feed's time is the feed's time.
    case edit(feedID: UUID, componentID: UUID)

    /// The part `.edit` opens, nil for every other mode.
    public var editTarget: PartEditTarget? {
        guard case let .edit(feedID, componentID) = self else { return nil }
        return .bottle(feedID: feedID, componentID: componentID)
    }
}

/// Every string this sheet shows, out of the view so copy is assertable
/// without a simulator.
public enum BottleSheetCopy {
    public static let title = LoggingL10n.text("Bottle")
    public static let cancel = LoggingL10n.text("Cancel")
    public static let contentsLabel = LoggingL10n.text("Contents")
    public static let breastMilk = LoggingL10n.text("Breast milk")
    public static let formula = LoggingL10n.text("Formula")
    public static let mixed = LoggingL10n.text("Mixed")
    public static let startBottle = LoggingL10n.text("Start bottle")
    /// The time row's label. It sits with the amounts because it times only
    /// what they log: Start bottle starts the bottle now.
    public static let drunkAt = LoggingL10n.text("Drunk at")
    public static let doneLabel = LoggingL10n.text("Done")
    /// `.start`'s button under the amount: logs a finished bottle.
    public static let logLabel = LoggingL10n.text("Log")
    public static let setAmount = LoggingL10n.text("Set amount")
    public static let volumeLabel = LoggingL10n.text("Volume")
    public static let decreaseAmount = LoggingL10n.text("Decrease amount")
    public static let increaseAmount = LoggingL10n.text("Increase amount")

    /// The amounts are bare numbers; this line above them names the unit.
    public static func orLogWhatWasDrunk(in unit: String) -> String {
        LoggingL10n.format("or log what was drunk, in %@", unit)
    }

    public static func bottleStopped(at time: String) -> String {
        LoggingL10n.format("Bottle stopped · %@", time)
    }
}

/// Backs every mode of `BottleSheet`. Contents and volume start from what
/// history already says, so the common taps — Start bottle, Log — need no
/// setup first.
@MainActor @Observable
public final class BottleSheetModel {
    private let store: BabyStore
    private let babyID: UUID
    public let unit: VolumeUnit
    public let mode: BottleSheetMode

    public var contents: BottleContents
    public var volumeML: Double
    public var when: Date

    /// `[35, 65, 100, 135]` ml, or four whole ounces — the ml amounts are
    /// what 30–120 ml of water makes once the powder is in.
    public let chips: [Double]

    public init(store: BabyStore, babyID: UUID, unit: VolumeUnit = .millilitres, mode: BottleSheetMode) {
        self.store = store
        self.babyID = babyID
        self.unit = unit
        self.mode = mode
        self.when = store.now
        let defaults = LogDefaults(store: store)
        var contents = (try? defaults.lastBottleContents(for: babyID)) ?? LogDefaults.fallbackContents
        var volumeML = (try? defaults.lastBottleVolumeML(for: babyID)) ?? LogDefaults.fallbackVolumeML
        // The amount screens start from the bottle they finish, not from
        // history: what it was started with, or what it was saved as.
        switch mode {
        case .start, .edit:
            break
        case .stopped(let timerID):
            if let started = (try? store.activeTimer(id: timerID))?.bottleContents { contents = started }
        case .amount(let feedID):
            if let saved = try? store.bottlePart(onFeed: feedID) {
                contents = saved.contents
                if saved.volumeML > 0 { volumeML = saved.volumeML }
            }
        }
        self.contents = contents
        self.volumeML = volumeML
        self.chips = Self.chips(for: unit)
    }

    /// What 30, 60, 90 and 120 ml of water make once formula is mixed in:
    /// powder adds about 12% (1 scoop per 30 ml, 0.77 ml per gram), rounded
    /// to the 5 ml step. Breast milk uses the same chips. Ounces stay whole.
    /// Shared with the bottle part editor, so the two never differ.
    static func chips(for unit: VolumeUnit) -> [Double] {
        switch unit {
        case .millilitres: [35, 65, 100, 135]
        case .ounces: [1, 2, 3, 4].map { Volume(ounces: $0).millilitres }
        }
    }

    /// ±5 ml, or ±½ oz — the same step both the exact-entry stepper and the
    /// stopped/amount mode's − and + buttons move by.
    public var stepAmount: Double { Self.stepAmount(for: unit) }

    static func stepAmount(for unit: VolumeUnit) -> Double {
        unit == .millilitres ? 5 : Volume(ounces: 0.5).millilitres
    }

    /// Save is off at 0 ml — a zero-volume bottle renders on Home and in
    /// History while every total that gates on `> 0` leaves it out.
    public var canDone: Bool { volumeML > 0 }

    /// "or log what was drunk, in ml": the chips carry no unit of their own.
    public var amountsCaption: String { BottleSheetCopy.orLogWhatWasDrunk(in: unit.shortName) }

    /// What a chip, or the live volume, reads in this sheet's unit.
    public func label(for ml: Double) -> String { Self.label(for: ml, unit: unit) }

    static func label(for ml: Double, unit: VolumeUnit) -> String {
        switch unit {
        case .millilitres: String(Int(ml.rounded()))
        case .ounces: String(Int((ml / Volume(ounces: 1).millilitres).rounded()))
        }
    }

    /// Starts a timed bottle. Reuses the feed's Live Activity when there is
    /// one — running, or in its break — rather than stacking a second card.
    public func startBottle() async throws {
        let timer = try store.startPart(.bottle(contents), for: babyID)
        await LiveActivityController.adoptOrStart(for: timer, in: store)
    }

    /// Logs a finished bottle of `ml` right now (or at `when`), joining the
    /// open feed if the break rules say it should.
    public func logChip(_ ml: Double) throws {
        _ = try store.logBottle(volumeML: ml, contents: contents, for: babyID, at: when)
    }

    public func step(_ direction: Int) {
        volumeML = max(0, volumeML + Double(direction) * stepAmount)
    }

    /// `.stopped` finishes the bottle and ends the feed in one write, then
    /// moves the feed's card into its break. `.amount` sets the volume and contents
    /// on a feed already logged. `.start` has nothing to finish — Log and
    /// Start bottle do their own writes — and `.edit` saves through its
    /// `PartEditModel`.
    public func done() async throws {
        switch mode {
        case .start, .edit:
            break
        case .stopped(let timerID):
            guard let timer = try store.activeTimer(id: timerID) else { throw BottleTimerError.timerChanged }
            let volumeML = volumeML
            let contents = contents
            try await LiveActivityController.stopping(timer, in: store) {
                _ = try store.stopBottle(timer, volumeML: volumeML, contents: contents)
            }
        case .amount(let feedID):
            try store.setBottle(volumeML: volumeML, contents: contents, onFeed: feedID)
        }
    }
}

/// One view across four modes: start a bottle or log one already drunk, or —
/// after a timed bottle stops, or to set an already-logged bottle's amount —
/// dial in how much and confirm once. Every mode sets the amount with the
/// bottle part editor's own block. `.edit` is that editor, with contents,
/// time, Save and Delete.
public struct BottleSheet: View {
    @State private var model: BottleSheetModel
    /// Present in `.edit` mode only.
    @State private var editModel: PartEditModel?
    private let onDone: () -> Void

    /// Start bottle and Done write, then await the Live Activity; a second
    /// tap in that gap would find the timer already gone and report a
    /// failure for a save that worked.
    @State private var isSaving = false
    @Environment(\.errorReporter) private var errorReporter

    public init(
        store: BabyStore,
        babyID: UUID,
        unit: VolumeUnit = .millilitres,
        mode: BottleSheetMode,
        onDone: @escaping () -> Void
    ) {
        _model = State(initialValue: BottleSheetModel(store: store, babyID: babyID, unit: unit, mode: mode))
        _editModel = State(initialValue: mode.editTarget.map {
            PartEditModel(store: store, babyID: babyID, target: $0, unit: unit)
        })
        self.onDone = onDone
    }

    private var isStartMode: Bool { model.mode == .start }

    public var body: some View {
        if let editModel {
            PartEditSheet(model: editModel, onDone: onDone)
        } else {
            logBody
        }
    }

    /// Presented with `fittedSheet`: as tall as this, so the title and
    /// Cancel are drawn here. Log (or Done) is the sheet's one primary action,
    /// pinned in the footer.
    private var logBody: some View {
        SheetScaffold(
            closeTitle: BottleSheetCopy.cancel, onClose: onDone,
            primary: SheetAction(isStartMode ? BottleSheetCopy.logLabel : BottleSheetCopy.doneLabel,
                                 identifier: isStartMode ? "bottle.log" : "bottle.done",
                                 isEnabled: model.canDone,
                                 action: isStartMode ? { logChip(model.volumeML) } : doneTapped)
        ) {
            PanelTitle(icon: .bottle, tint: Palette.feed, label: BottleSheetCopy.title)
        } content: {
            if isStartMode {
                startBody
            } else {
                amountBody
            }
        }
        .tint(Palette.feed)
    }

    // MARK: - .start

    @ViewBuilder private var startBody: some View {
        contentsPicker

        Button(action: startBottleTapped) {
            Text(BottleSheetCopy.startBottle)
                .font(.title3.weight(.bold))
                .fixedSize(horizontal: false, vertical: true)
        }
        .buttonStyle(.tile(Palette.feed, size: .large))
        .accessibilityIdentifier("bottle.start")

        // Never squeezed: a stated sheet height that comes up short
        // must not be absorbed by truncating the copy.
        Text(model.amountsCaption)
            .textRole(.caption)
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)

        // Below Start bottle, with the amounts: it times what they log,
        // and only that. Start bottle starts now.
        CardSection {
            DatePicker(BottleSheetCopy.drunkAt, selection: $model.when, displayedComponents: [.hourAndMinute])
                .accessibilityIdentifier("bottle.time")
        }

        amountSection
    }

    // MARK: - .stopped / .amount

    @ViewBuilder private var amountBody: some View {
        Text(caption)
            .font(.subheadline.weight(.semibold))
            .foregroundStyle(Palette.feed)
            .accessibilityIdentifier("bottle.caption")

        contentsPicker

        amountSection
    }

    /// Breast milk, Formula or Mixed. On the amount screens it is what the
    /// bottle held, corrected before it is saved.
    private var contentsPicker: some View {
        Picker(BottleSheetCopy.contentsLabel, selection: $model.contents) {
            Text(BottleSheetCopy.breastMilk).tag(BottleContents.breastMilk)
            Text(BottleSheetCopy.formula).tag(BottleContents.formula)
            Text(BottleSheetCopy.mixed).tag(BottleContents.mixed)
        }
        .pickerStyle(.segmented)
        .accessibilityIdentifier("bottle.contents")
    }

    private var caption: String {
        switch model.mode {
        case .start: ""
        case .stopped: BottleSheetCopy.bottleStopped(at: Date().formatted(date: .omitted, time: .shortened))
        case .amount: BottleSheetCopy.setAmount
        case .edit: ""
        }
    }

    // MARK: - Shared

    /// The bottle edit panel's amount block, so every screen that sets a
    /// bottle's amount looks and works the same: a chip dials in, the
    /// button under it writes.
    private var amountSection: some View {
        BottleAmountSection(volumeML: $model.volumeML, unit: model.unit, chips: model.chips,
                            label: model.label(for:), step: model.step, identifierPrefix: "bottle")
    }

    // MARK: - Actions

    private func startBottleTapped() {
        guard !isSaving else { return }
        isSaving = true
        Task {
            defer { isSaving = false }
            do {
                try await model.startBottle()
                errorReporter.clear()
                onDone()
            } catch {
                errorReporter.report(ErrorBannerCopy.saveFailed(what: ErrorSubject.bottle), error: error, category: AppLog.store)
            }
        }
    }

    private func logChip(_ ml: Double) {
        do {
            try model.logChip(ml)
            errorReporter.clear()
            onDone()
        } catch {
            errorReporter.report(ErrorBannerCopy.saveFailed(what: ErrorSubject.bottle), error: error, category: AppLog.store)
        }
    }

    private func doneTapped() {
        guard !isSaving else { return }
        isSaving = true
        Task {
            defer { isSaving = false }
            do {
                try await model.done()
                errorReporter.clear()
                onDone()
            } catch {
                errorReporter.report(ErrorBannerCopy.saveFailed(what: ErrorSubject.bottle), error: error, category: AppLog.store)
            }
        }
    }
}
