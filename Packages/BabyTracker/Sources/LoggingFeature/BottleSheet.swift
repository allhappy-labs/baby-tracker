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
/// history already says, so the common taps — Start bottle, a chip — need no
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

    /// `[30, 60, 90, 120]` ml, or the same four amounts converted from whole
    /// ounces — never a separate oz table, so the two units never drift.
    public let chips: [Double]

    public init(store: BabyStore, babyID: UUID, unit: VolumeUnit = .millilitres, mode: BottleSheetMode) {
        self.store = store
        self.babyID = babyID
        self.unit = unit
        self.mode = mode
        self.when = store.now
        let defaults = LogDefaults(store: store)
        self.contents = (try? defaults.lastBottleContents(for: babyID)) ?? LogDefaults.fallbackContents
        self.volumeML = (try? defaults.mostUsedBottleVolumeML(for: babyID)) ?? LogDefaults.fallbackVolumeML
        self.chips = Self.chips(for: unit)
    }

    /// `[30, 60, 90, 120]` ml, or the same four amounts converted from whole
    /// ounces. Shared with the bottle part editor, so the two never differ.
    static func chips(for unit: VolumeUnit) -> [Double] {
        switch unit {
        case .millilitres: [30, 60, 90, 120]
        case .ounces: [1, 2, 3, 4].map { Volume(ounces: $0).millilitres }
        }
    }

    /// ±10 ml, or ±½ oz — the same step both the exact-entry stepper and the
    /// stopped/amount mode's − and + buttons move by.
    public var stepAmount: Double { Self.stepAmount(for: unit) }

    static func stepAmount(for unit: VolumeUnit) -> Double {
        unit == .millilitres ? 10 : Volume(ounces: 0.5).millilitres
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
    /// moves the feed's card into its break. `.amount` only sets the volume
    /// on a feed already logged. `.start` has nothing to finish — chips and
    /// Start bottle do their own writes — and `.edit` saves through its
    /// `PartEditModel`.
    public func done() async throws {
        switch mode {
        case .start, .edit:
            break
        case .stopped(let timerID):
            guard let timer = try store.activeTimer(id: timerID) else { throw BottleTimerError.timerChanged }
            let volumeML = volumeML
            try await LiveActivityController.stopping(timer, in: store) {
                _ = try store.stopBottle(timer, volumeML: volumeML)
            }
        case .amount(let feedID):
            try store.setBottleVolume(volumeML, onFeed: feedID)
        }
    }
}

/// One view across four modes: start a bottle or log one instantly, or —
/// after a timed bottle stops, or to set an already-logged bottle's amount —
/// dial in how much and confirm once. `.edit` is the bottle part editor:
/// the same chips, with contents, time, Save and Delete.
public struct BottleSheet: View {
    @State private var model: BottleSheetModel
    /// Present in `.edit` mode only.
    @State private var editModel: PartEditModel?
    private let onDone: () -> Void

    /// The exact-amount field a long press on a chip reveals, in `.start`
    /// mode only. View state, not model state: it is how this screen is
    /// drawn right now, not a fact the model needs to remember.
    @State private var isEnteringExact = false
    /// Start bottle and Done write, then await the Live Activity; a second
    /// tap in that gap would find the timer already gone and report a
    /// failure for a save that worked.
    @State private var isSaving = false
    @Environment(\.errorReporter) private var errorReporter
    /// Four amounts share a row at the ordinary sizes; at the accessibility
    /// sizes a quarter of the sheet stacked "120" one digit per line.
    @Environment(\.dynamicTypeSize) private var typeSize

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

    private var logBody: some View {
        NavigationStack {
            ScrollView {
                Group {
                    if isStartMode {
                        startBody
                    } else {
                        amountBody
                    }
                }
                .padding(20)
            }
            // Scrolls rather than clips when the text outgrows the sheet's
            // stated height; at the ordinary sizes everything fits and it
            // does not bounce.
            .scrollBounceBehavior(.basedOnSize)
            .sheetHeight(height)
            .navigationTitle(BottleSheetCopy.title)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button(BottleSheetCopy.cancel, action: onDone)
                }
            }
        }
    }

    /// The stopped bottle's amount needs 380: at 330, Done sat under the
    /// bottom of the sheet once the content stopped being squeezed to fit.
    private var height: CGFloat {
        if isStartMode { isEnteringExact ? 360 : 340 }
        else { 380 }
    }

    // MARK: - .start

    private var startBody: some View {
        VStack(spacing: 14) {
            Picker(BottleSheetCopy.contentsLabel, selection: $model.contents) {
                Text(BottleSheetCopy.breastMilk).tag(BottleContents.breastMilk)
                Text(BottleSheetCopy.formula).tag(BottleContents.formula)
                Text(BottleSheetCopy.mixed).tag(BottleContents.mixed)
            }
            .pickerStyle(.segmented)
            .accessibilityIdentifier("bottle.contents")

            Button(action: startBottleTapped) {
                Text(BottleSheetCopy.startBottle)
                    .font(.title3.weight(.bold))
                    .fixedSize(horizontal: false, vertical: true)
            }
            .buttonStyle(.primaryFill(minHeight: 64))
            .accessibilityIdentifier("bottle.start")

            if !isEnteringExact {
                // Never squeezed: a stated sheet height that comes up short
                // must not be absorbed by truncating the copy.
                Text(model.amountsCaption)
                    .font(.footnote)
                    .foregroundStyle(Palette.secondaryText)
                    .fixedSize(horizontal: false, vertical: true)
            }

            // Below Start bottle, with the amounts: it times what they log,
            // and only that. Start bottle starts now.
            DatePicker(BottleSheetCopy.drunkAt, selection: $model.when, displayedComponents: [.hourAndMinute])
                .accessibilityIdentifier("bottle.time")

            if isEnteringExact {
                exactEntry
            } else {
                chipsRow(selects: false)
            }
        }
    }

    private var exactEntry: some View {
        VStack(spacing: 10) {
            Stepper(
                value: $model.volumeML,
                in: 0...max(400, model.volumeML + model.stepAmount),
                step: model.stepAmount
            ) {
                BottleVolumeField(volumeML: $model.volumeML, unit: model.unit, label: BottleSheetCopy.volumeLabel)
            }
            .accessibilityLabel(BottleSheetCopy.volumeLabel)

            Button(BottleSheetCopy.doneLabel) { logChip(model.volumeML) }
                .buttonStyle(.primaryFill(minHeight: 56))
                .disabled(!model.canDone)
                .accessibilityIdentifier("bottle.exact")
        }
    }

    // MARK: - .stopped / .amount

    private var amountBody: some View {
        VStack(spacing: 14) {
            Text(caption)
                .font(.subheadline.weight(.semibold))
                .foregroundStyle(Palette.feed)
                .accessibilityIdentifier("bottle.caption")

            HStack(alignment: .firstTextBaseline, spacing: 6) {
                Text(model.label(for: model.volumeML))
                    .numeralStyle(.display)
                Text(model.unit.shortName)
                    .font(.title3)
                    .foregroundStyle(Palette.secondaryText)
            }
            .accessibilityElement(children: .combine)
            .accessibilityIdentifier("bottle.volume")

            chipsRow(selects: true)

            HStack(spacing: 8) {
                Button {
                    model.step(-1)
                } label: {
                    Image(systemName: "minus").frame(maxWidth: .infinity, minHeight: 44)
                }
                .buttonStyle(.tracker(Palette.secondaryText))
                .accessibilityLabel(BottleSheetCopy.decreaseAmount)
                .accessibilityIdentifier("bottle.minus")

                Button {
                    model.step(1)
                } label: {
                    Image(systemName: "plus").frame(maxWidth: .infinity, minHeight: 44)
                }
                .buttonStyle(.tracker(Palette.secondaryText))
                .accessibilityLabel(BottleSheetCopy.increaseAmount)
                .accessibilityIdentifier("bottle.plus")
            }

            Button(BottleSheetCopy.doneLabel, action: doneTapped)
                .buttonStyle(.primaryFill(minHeight: 56))
                .disabled(!model.canDone)
                .accessibilityIdentifier("bottle.done")
        }
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

    /// `selects` is false in `.start` mode, where a tap logs instantly and a
    /// long press opens the exact field, and true in `.stopped`/`.amount`,
    /// where a tap only dials in the amount and there is no long press.
    @ViewBuilder
    private func chipsRow(selects: Bool) -> some View {
        let columns = typeSize.isAccessibilitySize ? 2 : model.chips.count
        LazyVGrid(columns: Array(repeating: GridItem(.flexible(), spacing: 8), count: columns), spacing: 8) {
            ForEach(Array(model.chips.enumerated()), id: \.offset) { index, ml in
                if selects {
                    chip(ml, index: index, selects: true)
                } else {
                    chip(ml, index: index, selects: false)
                        // High-priority, not simultaneous: a long press must
                        // pre-empt the chip's own tap, or the same touch
                        // would both log the chip and open the exact field.
                        .highPriorityGesture(
                            LongPressGesture().onEnded { _ in isEnteringExact = true }
                        )
                }
            }
        }
    }

    private func chip(_ ml: Double, index: Int, selects: Bool) -> some View {
        Button {
            if selects {
                model.volumeML = ml
            } else {
                logChip(ml)
            }
        } label: {
            Text(model.label(for: ml))
                .font(.body.weight(.bold))
                .frame(maxWidth: .infinity, minHeight: 44)
        }
        .buttonStyle(.tracker(Palette.feed, emphasis: selects && model.volumeML == ml ? .primary : .secondary))
        .accessibilityIdentifier("bottle.chip.\(index)")
        .accessibilityLabel("\(model.label(for: ml)) \(model.unit.shortName)")
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
