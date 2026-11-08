import SwiftUI
import Observation
import BabyCore
import BabyStore
import BabyUI

public enum PumpPanelCopy {
    public static var title: String { Tracker.pump.title }
    public static let start = LoggingL10n.text("Start")
    public static let editRunning = LoggingL10n.text("Edit running")
    public static let amount = LoggingL10n.text("Amount")
    public static let tooLong = LoggingL10n.text("A pumping session can be at most 6 hours.")
    /// The time row's label, with the amounts: it times only what Log writes.
    public static let pumpedAt = LoggingL10n.text("Pumped at")

    /// The amounts are bare numbers; this line above them names the unit.
    public static func orLogWhatWasPumped(in unit: String) -> String {
        LoggingL10n.format("or log what was pumped, in %@", unit)
    }
}

/// The pump screens' amount control: the bottle's, with its own four amounts.
@MainActor enum PumpAmountControl {
    /// 30, 60, 90 and 120 ml, or 1–4 whole ounces. Pumped milk has no powder
    /// in it, so the bottle's formula-displacement amounts do not apply.
    static func chips(for unit: VolumeUnit) -> [Double] {
        switch unit {
        case .millilitres: [30, 60, 90, 120]
        case .ounces: [1, 2, 3, 4].map { Volume(ounces: $0).millilitres }
        }
    }

    /// The bottle's ±5 ml or ±½ oz, kept inside what the server takes.
    static func stepped(_ volumeML: Double, by direction: Int, unit: VolumeUnit) -> Double {
        min(PumpRules.maxVolumeML, max(0, volumeML + Double(direction) * BottleSheetModel.stepAmount(for: unit)))
    }

    /// 0 is no amount: the screens treat an empty amount and 0 ml alike.
    static func amount(_ volumeML: Double) -> Double? {
        PumpRules.roundedVolume(volumeML) > 0 ? volumeML : nil
    }
}

public enum PumpPanelMode: Equatable, Sendable {
    /// The Pump Home action: a side, then Start, now.
    case start
    /// Past entry → Pump: a finished session.
    case past
    /// History, the strip, or an Ask source: one session, running or not.
    case edit(UUID)
}

/// The pump panel, laid out as the bottle's. Start mode is the bottle
/// sheet's: side, Start, then a finished session logged from an amount.
/// Past entry and edit are the bottle part editor's: the amount once the
/// session has ended, side, Start and End (End reads Running while live),
/// and the note under Details.
///
/// Edit mode holds the session's id, not the session: the other phone may
/// stop or delete it while the panel is open. Every write fetches it afresh.
@MainActor @Observable
public final class PumpPanelModel {
    public let store: BabyStore
    public let mode: PumpPanelMode
    public let unit: VolumeUnit
    public var side: PumpSide = .both
    public var start: Date
    public var end: Date?
    /// The amount, set with the bottle's control. 0 is no amount.
    public var volumeML: Double = 0
    /// Start mode: when the session Log writes ended. Start starts now.
    public var when: Date
    public var note = ""
    /// Start mode: the family's running pump, if any. The panel then offers
    /// only Edit running, whichever phone started it.
    public private(set) var runningPumpID: UUID?
    /// Edit mode: the session has no end yet.
    public private(set) var isRunning = false
    public private(set) var isGone = false
    public private(set) var latestAllowedDate: Date
    private let writeContext: RealmWriteContext?
    /// The session's amount as the panel last took it from the store. While
    /// the panel still shows exactly this, the caregiver has not touched the
    /// amount, and an amount entered on the other phone is adopted.
    private var loadedAmount: Double?

    public init(store: BabyStore, mode: PumpPanelMode, unit: VolumeUnit = .millilitres) {
        self.store = store
        self.mode = mode
        self.unit = unit
        let now = store.now
        start = now
        when = now
        latestAllowedDate = now
        writeContext = try? store.captureWriteContext()
        switch mode {
        case .start:
            runningPumpID = (try? store.runningPump())?.id
            // The last amount is the likeliest next one, as for a bottle.
            volumeML = (try? store.lastPumpVolumeML()) ?? 0
        case .past:
            start = now.addingTimeInterval(-20 * 60)
            end = now
        case .edit(let id):
            guard let pump = try? store.pump(id: id) else {
                isGone = true
                return
            }
            side = pump.side
            start = pump.startedAt
            end = pump.endedAt
            isRunning = pump.isRunning
            volumeML = pump.volumeML ?? 0
            loadedAmount = PumpAmountControl.amount(volumeML)
            note = pump.note ?? ""
            latestAllowedDate = max(now, pump.endedAt ?? pump.startedAt)
        }
    }

    public var isEditing: Bool {
        if case .edit = mode { return true }
        return false
    }

    /// The edit panel's amount shows once the session has ended. Start mode
    /// shows its own, for Log.
    public var showsAmount: Bool { mode != .start && !isRunning }

    /// What saving writes: nil when the amount reads 0.
    public var amount: Double? { PumpAmountControl.amount(volumeML) }
    public var hasAmount: Bool { amount != nil }

    public var chips: [Double] { PumpAmountControl.chips(for: unit) }
    public func label(for ml: Double) -> String { BottleSheetModel.label(for: ml, unit: unit) }
    public func step(_ direction: Int) { volumeML = PumpAmountControl.stepped(volumeML, by: direction, unit: unit) }

    /// "or log what was pumped, in ml": the chips carry no unit of their own.
    public var amountsCaption: String { PumpPanelCopy.orLogWhatWasPumped(in: unit.shortName) }

    public var validationMessage: String? {
        guard mode != .start else { return nil }
        if let problem = TimeSpan(start: start, end: end).problem(latest: latestAllowedDate) {
            return TimeSpanCopy.message(problem)
        }
        if let end, end.timeIntervalSince(start) > PumpRules.maxSeconds { return PumpPanelCopy.tooLong }
        return nil
    }

    public var canSave: Bool {
        guard writeContext != nil, !isGone else { return false }
        if mode == .start { return runningPumpID == nil }
        guard validationMessage == nil, isRunning || end != nil else { return false }
        return PumpRules.isValidVolume(PumpRules.roundedVolume(volumeML))
    }

    /// Start mode's Log, off at 0 as the bottle's is: a session logged from
    /// here is its amount. Past entry logs one without.
    public var canLog: Bool {
        guard mode == .start, writeContext != nil, runningPumpID == nil else { return false }
        return hasAmount && PumpRules.isValidVolume(PumpRules.roundedVolume(volumeML))
    }

    /// The panel's Start. Async so the Lock Screen card can start with it
    /// (Task 12).
    @discardableResult
    public func startNow() async throws -> PumpEntry {
        guard mode == .start, let writeContext else { throw EditModeError.invalid }
        let pump = try store.startPump(side: side, expectedWriteContext: writeContext)
        runningPumpID = pump.id
        await PumpActivityController.start(for: pump)
        return pump
    }

    /// Start mode's Log: a finished session of the amount at `when`, as the
    /// bottle sheet's Log writes a finished bottle.
    public func logNow() throws {
        guard canLog, let writeContext else { throw EditModeError.invalid }
        try store.logPump(side: side, from: when, to: when, volumeML: amount, note: nil, expectedWriteContext: writeContext)
    }

    public func save() throws {
        guard let writeContext else { throw EditModeError.invalid }
        switch mode {
        case .start:
            throw EditModeError.invalid
        case .past:
            guard let end, canSave else { throw EditModeError.invalid }
            try store.logPump(side: side, from: start, to: end, volumeML: amount, note: note, expectedWriteContext: writeContext)
        case .edit(let id):
            guard let pump = try store.pump(id: id) else {
                isGone = true
                throw EditModeError.gone
            }
            // Stopped on the other phone since the panel last looked: take
            // its end rather than send a running state for a finished session.
            if isRunning, !pump.isRunning { reload() }
            guard canSave else { throw EditModeError.invalid }
            try store.updatePump(pump, side: side, startedAt: start, endedAt: isRunning ? nil : end,
                                 volumeML: isRunning ? pump.volumeML : amount, note: note, expectedWriteContext: writeContext)
        }
    }

    public func delete() throws {
        guard case .edit(let id) = mode else { throw EditModeError.invalid }
        guard let pump = try store.pump(id: id) else {
            isGone = true
            throw EditModeError.gone
        }
        try store.softDelete(pump)
    }

    /// After any write, this phone's or the other's: a start made elsewhere
    /// turns Start into Edit running; a stop made elsewhere turns Running
    /// into the real End; a delete closes the panel.
    public func reload() {
        switch mode {
        case .start:
            runningPumpID = (try? store.runningPump())?.id
        case .past:
            break
        case .edit(let id):
            guard let pump = try? store.pump(id: id) else {
                isGone = true
                return
            }
            if isRunning, !pump.isRunning {
                isRunning = false
                end = pump.endedAt
                latestAllowedDate = max(store.now, pump.endedAt ?? pump.startedAt)
            }
            // An amount entered on the other phone, unless one was typed here:
            // saving this panel must never send "no amount" over theirs.
            let stored = PumpAmountControl.amount(pump.volumeML ?? 0)
            if amount == loadedAmount, stored != loadedAmount {
                volumeML = pump.volumeML ?? 0
                loadedAmount = stored
            }
        }
    }
}

/// The panel's rows, in the bottle part editor's order: the amount once
/// ended, side, then Start and End, and Details. Start mode shows the side
/// alone. Shared by the pump sheet and Past entry's Pump option.
struct PumpFields: View {
    @Bindable var model: PumpPanelModel
    @Binding var isShowingDetails: Bool

    var body: some View {
        if model.showsAmount {
            PumpAmountFields(volumeML: $model.volumeML, model: model, identifierPrefix: "pump")
        }

        // No card behind a segmented control: it is a container already.
        Picker(PartEditCopy.sideLabel, selection: $model.side) {
            ForEach(PumpSide.allCases, id: \.self) { side in
                Text(side.displayName).tag(side)
            }
        }
        .pickerStyle(.segmented)
        .accessibilityIdentifier("pump.side")

        if model.mode != .start {
            TimeSpanSection(start: $model.start, end: $model.end, endStyle: model.isRunning ? .running : .required,
                            latest: model.latestAllowedDate, identifierPrefix: "pump") {
                if let message = model.validationMessage {
                    InlineError(message)
                        .accessibilityIdentifier("pump.validation")
                }
            }
        }

        if model.mode != .start {
            DetailsSection(isExpanded: $isShowingDetails, identifier: "pump.details") {
                DetailFieldRow(EditChromeCopy.note, text: $model.note, identifier: "pump.note")
            }
        }
    }
}

/// The bottle's amount control in the pump's colour.
struct PumpAmountFields: View {
    @Binding var volumeML: Double
    let unit: VolumeUnit
    let label: (Double) -> String
    let step: (Int) -> Void
    let identifierPrefix: String

    init(volumeML: Binding<Double>, model: PumpPanelModel, identifierPrefix: String) {
        _volumeML = volumeML
        unit = model.unit
        label = model.label(for:)
        step = model.step
        self.identifierPrefix = identifierPrefix
    }

    init(volumeML: Binding<Double>, model: PumpAmountModel, identifierPrefix: String) {
        _volumeML = volumeML
        unit = model.unit
        label = model.label(for:)
        step = model.step
        self.identifierPrefix = identifierPrefix
    }

    var body: some View {
        BottleAmountSection(volumeML: $volumeML, unit: unit, chips: PumpAmountControl.chips(for: unit),
                            label: label, step: step, identifierPrefix: identifierPrefix,
                            tint: Palette.pump, volumeLabel: PumpPanelCopy.amount)
    }
}

public struct PumpSheet: View {
    @State private var model: PumpPanelModel
    @State private var isShowingDetails: Bool
    /// `onDone` runs once even when a delete is followed by the revision
    /// bump that marks the session gone.
    @State private var isFinished = false
    private let onDone: () -> Void
    private let onSwitchTo: ((LogSheet) -> Void)?
    @Environment(\.errorReporter) private var errorReporter

    public init(store: BabyStore, mode: PumpPanelMode, unit: VolumeUnit = .millilitres,
                onDone: @escaping () -> Void, onSwitchTo: ((LogSheet) -> Void)? = nil) {
        let model = PumpPanelModel(store: store, mode: mode, unit: unit)
        _model = State(initialValue: model)
        _isShowingDetails = State(initialValue: !model.note.isEmpty)
        self.onDone = onDone
        self.onSwitchTo = onSwitchTo
    }

    public var body: some View {
        EntryPanel(title: PumpPanelCopy.title, icon: .pump, tint: Palette.pump, canSave: model.canSave,
                   identifierPrefix: "pump", onCancel: onDone,
                   onSave: saveAction, onDelete: deleteAction, logAction: logAction) {
            if model.mode == .start, let running = model.runningPumpID {
                Button { onSwitchTo?(.editEntry(.pump(running))) } label: {
                    Text(PumpPanelCopy.editRunning)
                        .font(.title3.weight(.bold))
                        .fixedSize(horizontal: false, vertical: true)
                }
                .buttonStyle(.tile(Palette.pump, size: .large))
                .accessibilityIdentifier("pump.editRunning")
            } else {
                PumpFields(model: model, isShowingDetails: $isShowingDetails)
                if model.mode == .start {
                    Button(action: start) {
                        Text(PumpPanelCopy.start)
                            .font(.title3.weight(.bold))
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    .buttonStyle(.tile(Palette.pump, size: .large))
                    .disabled(!model.canSave)
                    .accessibilityIdentifier("pump.startNow")

                    logSection
                }
            }
        }
        .editLifecycle(revision: model.store.revision, isGone: model.isGone, reload: model.reload, onGone: finish)
    }

    /// Below Start, as below Start bottle: a session already finished,
    /// logged from its amount. The time is Log's only; Start starts now.
    @ViewBuilder
    private var logSection: some View {
        Text(model.amountsCaption)
            .textRole(.caption)
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)

        CardSection {
            DatePicker(PumpPanelCopy.pumpedAt, selection: $model.when, displayedComponents: [.hourAndMinute])
                .accessibilityIdentifier("pump.time")
        }

        PumpAmountFields(volumeML: $model.volumeML, model: model, identifierPrefix: "pump")
    }

    /// Start mode's footer: Log, the sheet's one primary action; Start sits
    /// in the content above it.
    private var logAction: SheetAction? {
        guard model.mode == .start, model.runningPumpID == nil else { return nil }
        return SheetAction(BottleSheetCopy.logLabel, identifier: "pump.log", isEnabled: model.canLog, action: log)
    }

    /// Start mode's Start is in the content and its Log is `logAction`: no Save.
    private var saveAction: (() -> Void)? {
        guard model.mode != .start else { return nil }
        return save
    }

    private var deleteAction: (() -> Void)? {
        guard model.isEditing else { return nil }
        return delete
    }

    private func start() {
        Task {
            do {
                try await model.startNow()
                errorReporter.clear()
                finish()
            } catch {
                // The other phone started one a moment ago: show Edit running.
                model.reload()
                errorReporter.report(ErrorBannerCopy.saveFailed(what: ErrorSubject.pump), error: error, category: AppLog.store)
            }
        }
    }

    private func log() {
        if performEdit(model.logNow, failure: ErrorBannerCopy.saveFailed(what: ErrorSubject.pump), reporter: errorReporter) {
            finish()
        }
    }

    private func save() {
        if performEdit(model.save, failure: ErrorBannerCopy.saveFailed(what: ErrorSubject.pump), reporter: errorReporter) {
            finish()
        }
    }

    private func delete() {
        if performEdit(model.delete, failure: ErrorBannerCopy.deleteFailed(what: ErrorSubject.pump), reporter: errorReporter) {
            finish()
        }
    }

    private func finish() {
        guard !isFinished else { return }
        isFinished = true
        onDone()
    }
}
