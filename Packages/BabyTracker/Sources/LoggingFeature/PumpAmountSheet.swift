import SwiftUI
import Observation
import BabyCore
import BabyStore
import BabyUI

public enum PumpAmountCopy {
    public static let title = LoggingL10n.text("How much?")
    public static let skip = LoggingL10n.text("Skip")
    public static func stopped(at time: String) -> String { LoggingL10n.format("Pumping stopped · %@", time) }
}

/// The amount for a session just stopped, from the strip or the Lock Screen
/// card, laid out as a stopped bottle's: caption, side, the bottle's amount
/// control, Done. Skip leaves it empty, as it may stay.
@MainActor @Observable
public final class PumpAmountModel {
    private let store: BabyStore
    public let pumpID: UUID
    public let unit: VolumeUnit
    public var volumeML: Double
    /// What the session was started with, corrected before it is saved, as a
    /// stopped bottle's contents are.
    public var side: PumpSide
    public private(set) var endedAt: Date?
    public private(set) var isGone = false

    public init(store: BabyStore, pumpID: UUID, unit: VolumeUnit = .millilitres) {
        self.store = store
        self.pumpID = pumpID
        self.unit = unit
        let pump = try? store.pump(id: pumpID)
        endedAt = pump?.endedAt
        side = pump?.side ?? .both
        isGone = pump == nil
        // The last amount is the likeliest next one.
        volumeML = pump?.volumeML ?? (try? store.lastPumpVolumeML()) ?? 0
    }

    public func label(for ml: Double) -> String { BottleSheetModel.label(for: ml, unit: unit) }
    public func step(_ direction: Int) { volumeML = PumpAmountControl.stepped(volumeML, by: direction, unit: unit) }
    public var canSave: Bool { !isGone && PumpRules.isValidVolume(PumpRules.roundedVolume(volumeML)) }

    public func save() throws {
        guard let pump = try store.pump(id: pumpID) else {
            isGone = true
            throw EditModeError.gone
        }
        // Nothing entered is the same as Skip: no amount, not 0 ml.
        let amount = PumpAmountControl.amount(volumeML)
        if side == pump.side {
            try store.setPumpAmount(pump, volumeML: amount)
        } else {
            try store.updatePump(pump, side: side, startedAt: pump.startedAt, endedAt: pump.endedAt,
                                 volumeML: amount, note: pump.note)
        }
    }
}

/// Presented with `fittedSheet`: as tall as this, so the title and Skip are
/// drawn here rather than in a navigation bar.
public struct PumpAmountSheet: View {
    @State private var model: PumpAmountModel
    private let onDone: () -> Void
    @Environment(\.errorReporter) private var errorReporter

    public init(store: BabyStore, pumpID: UUID, unit: VolumeUnit = .millilitres, onDone: @escaping () -> Void) {
        _model = State(initialValue: PumpAmountModel(store: store, pumpID: pumpID, unit: unit))
        self.onDone = onDone
    }

    public var body: some View {
        // Done, as on a stopped bottle. It stays on at 0, which saves no
        // amount: a pump's amount may stay empty. Skip leaves it empty.
        SheetScaffold(
            PumpAmountCopy.title, closeTitle: PumpAmountCopy.skip, onClose: nil,
            primary: SheetAction(BottleSheetCopy.doneLabel, identifier: "pumpAmount.save",
                                 isEnabled: model.canSave, action: save),
            secondary: SheetAction(PumpAmountCopy.skip, identifier: "pumpAmount.skip", action: onDone)
        ) {
            if let ended = model.endedAt {
                Text(PumpAmountCopy.stopped(at: ended.formatted(date: .omitted, time: .shortened)))
                    .font(.subheadline.weight(.semibold))
                    .foregroundStyle(Palette.pump)
                    .fixedSize(horizontal: false, vertical: true)
                    .accessibilityIdentifier("pumpAmount.caption")
            }
            // No card behind a segmented control: it is a container already.
            Picker(PartEditCopy.sideLabel, selection: $model.side) {
                ForEach(PumpSide.allCases, id: \.self) { side in
                    Text(side.displayName).tag(side)
                }
            }
            .pickerStyle(.segmented)
            .accessibilityIdentifier("pumpAmount.side")

            PumpAmountFields(volumeML: $model.volumeML, model: model, identifierPrefix: "pumpAmount")
        }
        .tint(Palette.pump)
    }

    private func save() {
        if performEdit(model.save, failure: ErrorBannerCopy.saveFailed(what: ErrorSubject.pump), reporter: errorReporter) {
            onDone()
        }
    }
}
