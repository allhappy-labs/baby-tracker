import SwiftUI
import BabyCore
import BabyStore
import BabyUI

public enum PastEntrySheetCopy {
    public static let title = LoggingL10n.text("Past entry")
    public static let cancel = LoggingL10n.text("Cancel")
    public static let kindLabel = LoggingL10n.text("Kind")
    public static let startLabel = LoggingL10n.text("Start")
    public static let endLabel = LoggingL10n.text("End")
    public static let volumeLabel = LoggingL10n.text("Volume")
    public static let contentsLabel = LoggingL10n.text("Contents")
    public static let save = LoggingL10n.text("Save")
    public static let leftBreast = LoggingL10n.text("Left breast")
    public static let rightBreast = LoggingL10n.text("Right breast")
    public static let bottle = LoggingL10n.text("Bottle")
    public static let breastMilk = LoggingL10n.text("Breast milk")
    public static let formula = LoggingL10n.text("Formula")
    public static let mixed = LoggingL10n.text("Mixed")

    public static let endBeforeStart = LoggingL10n.text("End must be after start.")
    public static let inTheFuture = LoggingL10n.text("Time can’t be in the future.")
    public static let breastTooShort = LoggingL10n.text("A breast feed must be at least 30 seconds.")
}

/// A feed or sleep logged after the fact, for the one tracker without its own
/// quick-log sheet: something that already happened, start to end.
@MainActor @Observable
public final class PastEntryModel {
    public enum Kind: Sendable, CaseIterable, Hashable {
        case left, right, bottle, sleep
    }

    private let store: BabyStore
    private let babyID: UUID

    public var kind: Kind = .left
    public var start: Date
    public var end: Date
    public var volumeML: Double
    public var contents: BottleContents

    public init(store: BabyStore, babyID: UUID) {
        self.store = store
        self.babyID = babyID
        self.end = store.now
        self.start = store.now.addingTimeInterval(-600)
        let defaults = LogDefaults(store: store)
        self.volumeML = (try? defaults.mostUsedBottleVolumeML(for: babyID)) ?? LogDefaults.fallbackVolumeML
        self.contents = (try? defaults.lastBottleContents(for: babyID)) ?? LogDefaults.fallbackContents
    }

    private var isBreast: Bool { kind == .left || kind == .right }

    /// `nil` when the entry can be saved. The end must be after the start,
    /// neither may be in the future, and a breast part must be at least 30 s
    /// — the same floor `cleanedBreastComponents` applies when a running
    /// timer stops.
    public var validationMessage: String? {
        if end <= start { return PastEntrySheetCopy.endBeforeStart }
        if start > store.now || end > store.now { return PastEntrySheetCopy.inTheFuture }
        if isBreast && end.timeIntervalSince(start) < 30 { return PastEntrySheetCopy.breastTooShort }
        return nil
    }

    public func save() throws {
        switch kind {
        case .left: _ = try store.logBreast(side: .left, from: start, to: end, for: babyID)
        case .right: _ = try store.logBreast(side: .right, from: start, to: end, for: babyID)
        case .bottle: _ = try store.logBottle(volumeML: volumeML, contents: contents, for: babyID, at: start)
        case .sleep: _ = try store.logSleep(from: start, to: end, for: babyID)
        }
    }
}

public struct PastEntrySheet: View {
    @State private var model: PastEntryModel
    private let unit: VolumeUnit
    private let onDone: () -> Void
    @Environment(\.errorReporter) private var errorReporter

    public init(store: BabyStore, babyID: UUID, unit: VolumeUnit = .millilitres, onDone: @escaping () -> Void) {
        _model = State(initialValue: PastEntryModel(store: store, babyID: babyID))
        self.unit = unit
        self.onDone = onDone
    }

    public var body: some View {
        NavigationStack {
            Form {
                Section {
                    Picker(PastEntrySheetCopy.kindLabel, selection: $model.kind) {
                        Text(PastEntrySheetCopy.leftBreast).tag(PastEntryModel.Kind.left)
                        Text(PastEntrySheetCopy.rightBreast).tag(PastEntryModel.Kind.right)
                        Text(PastEntrySheetCopy.bottle).tag(PastEntryModel.Kind.bottle)
                        Text(Tracker.sleep.title).tag(PastEntryModel.Kind.sleep)
                    }
                    .accessibilityIdentifier("pastEntry.kind")

                    DatePicker(PastEntrySheetCopy.startLabel, selection: $model.start)
                        .accessibilityIdentifier("pastEntry.start")
                    DatePicker(PastEntrySheetCopy.endLabel, selection: $model.end)
                        .accessibilityIdentifier("pastEntry.end")

                    if model.kind == .bottle {
                        BottleVolumeField(volumeML: $model.volumeML, unit: unit, label: PastEntrySheetCopy.volumeLabel)
                            .accessibilityIdentifier("pastEntry.volume")
                        Picker(PastEntrySheetCopy.contentsLabel, selection: $model.contents) {
                            Text(PastEntrySheetCopy.breastMilk).tag(BottleContents.breastMilk)
                            Text(PastEntrySheetCopy.formula).tag(BottleContents.formula)
                            Text(PastEntrySheetCopy.mixed).tag(BottleContents.mixed)
                        }
                        .accessibilityIdentifier("pastEntry.contents")
                    }
                } footer: {
                    if let message = model.validationMessage {
                        Text(message)
                            .foregroundStyle(Palette.destructive)
                            .accessibilityIdentifier("pastEntry.validation")
                    }
                }

                Section {
                    Button(PastEntrySheetCopy.save, action: saveTapped)
                        .disabled(model.validationMessage != nil)
                        .accessibilityIdentifier("pastEntry.save")
                }
            }
            .sheetHeight(height)
            .navigationTitle(PastEntrySheetCopy.title)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button(PastEntrySheetCopy.cancel, action: onDone)
                }
            }
        }
    }

    /// Kind, start and end are always on screen. Bottle adds its two rows;
    /// an invalid combination adds the footer's explanation. At 340 the
    /// Save row was cut by the bottom of the sheet.
    private var height: CGFloat {
        380
            + (model.kind == .bottle ? 100 : 0)
            + (model.validationMessage == nil ? 0 : 40)
    }

    private func saveTapped() {
        do {
            try model.save()
            errorReporter.clear()
            onDone()
        } catch {
            let subject = model.kind == .sleep ? ErrorSubject.sleep : ErrorSubject.feed
            errorReporter.report(ErrorBannerCopy.saveFailed(what: subject), error: error, category: AppLog.store)
        }
    }
}
