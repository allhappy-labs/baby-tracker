import SwiftUI
import BabyCore
import BabyStore
import BabyUI

public enum PastEntryCopy {
    public static let title = LoggingL10n.text("What happened?")
    public static let cancel = LoggingL10n.text("Cancel")
}

/// Something that already happened, logged after the fact. It opens on
/// "What happened?" — the six kinds as tiles — and a tap turns the sheet
/// into that kind's panel, with Back to the list. Each panel is the one the
/// kind is logged or edited with, so it gets the whole sheet; none shares a
/// form with a kind picker above it.
public struct PastEntrySheet: View {
    /// The kinds offered, in the order of the tiles.
    public static let kinds: [HomeActionKind] = [.breast, .bottle, .sleep, .pump, .diaper, .solids]

    private let store: BabyStore
    private let babyID: UUID
    private let unit: VolumeUnit
    private let onDone: () -> Void
    /// The kind picked, or none while the list shows.
    @State private var kind: HomeActionKind?
    @Environment(\.dynamicTypeSize) private var typeSize

    public init(store: BabyStore, babyID: UUID, unit: VolumeUnit = .millilitres, onDone: @escaping () -> Void) {
        self.store = store
        self.babyID = babyID
        self.unit = unit
        self.onDone = onDone
    }

    public var body: some View {
        switch kind {
        case .breast: NewPartPanel(store: store, babyID: babyID, unit: unit, kind: .breast, onBack: back, onDone: onDone)
        case .bottle: NewPartPanel(store: store, babyID: babyID, unit: unit, kind: .bottle, onBack: back, onDone: onDone)
        case .sleep: NewPartPanel(store: store, babyID: babyID, unit: unit, kind: .sleep, onBack: back, onDone: onDone)
        case .pump: PastPumpPanel(store: store, unit: unit, onBack: back, onDone: onDone)
        case .diaper: DiaperSheet(store: store, babyID: babyID, onBack: back, onDone: onDone)
        case .solids: SolidsSheet(store: store, babyID: babyID, onBack: back, onDone: onDone)
        default: list
        }
    }

    private func back() { kind = nil }

    private var columns: [GridItem] {
        Array(repeating: GridItem(.flexible(), spacing: Spacing.s), count: typeSize.isAccessibilitySize ? 1 : 3)
    }

    private var list: some View {
        SheetScaffold(PastEntryCopy.title, closeTitle: PastEntryCopy.cancel, onClose: onDone) {
            LazyVGrid(columns: columns, spacing: Spacing.s) {
                ForEach(Self.kinds, id: \.self) { kind in
                    tile(kind)
                }
            }
        }
    }

    private func tile(_ kind: HomeActionKind) -> some View {
        let glyph = BabyIconView(kind.icon, size: 24)
        // Never shrunk to fit and never cut: the words wrap.
        let label = Text(kind.title)
            .font(.subheadline.weight(.semibold))
            .fixedSize(horizontal: false, vertical: true)
        return Button { self.kind = kind } label: {
            if typeSize.isAccessibilitySize {
                HStack(spacing: Spacing.m) { glyph; label; Spacer(minLength: 0) }
            } else {
                VStack(spacing: Spacing.s) { glyph; label.multilineTextAlignment(.center) }
            }
        }
        .buttonStyle(.tile(kind.color, size: .large))
        .accessibilityIdentifier("pastEntry.\(kind.rawValue)")
    }
}

/// A breast side, bottle or sleep logged after the fact: the part panel in
/// new mode, its kind picked on the list before it.
struct NewPartPanel: View {
    @State private var model: NewPartModel
    @State private var isShowingDetails = false
    private let onBack: () -> Void
    private let onDone: () -> Void
    @Environment(\.errorReporter) private var errorReporter

    init(store: BabyStore, babyID: UUID, unit: VolumeUnit, kind: PartKind, onBack: @escaping () -> Void,
         onDone: @escaping () -> Void) {
        _model = State(initialValue: NewPartModel(store: store, babyID: babyID, unit: unit, kind: kind))
        self.onBack = onBack
        self.onDone = onDone
    }

    var body: some View {
        EntryPanel(title: title, icon: icon, tint: model.kind == .sleep ? Palette.sleep : Palette.feed,
                   canSave: model.canSave, identifierPrefix: "partEdit", onCancel: onBack,
                   cancelTitle: EditChromeCopy.back, onSave: save, onDelete: nil) {
            PartFields(model: model, isShowingDetails: $isShowingDetails)
        }
    }

    private var title: String {
        switch model.kind {
        case .breast: PartEditCopy.breastTitle
        case .bottle: PartEditCopy.bottleTitle
        case .sleep: Tracker.sleep.title
        }
    }

    private var icon: BabyIcon {
        switch model.kind {
        case .breast: model.side == .left ? .breastLeft : .breastRight
        case .bottle: .bottle
        case .sleep: .sleep
        }
    }

    private func save() {
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

/// A pump session that already ended, with its amount.
struct PastPumpPanel: View {
    @State private var model: PumpPanelModel
    @State private var isShowingDetails = false
    private let onBack: () -> Void
    private let onDone: () -> Void
    @Environment(\.errorReporter) private var errorReporter

    init(store: BabyStore, unit: VolumeUnit, onBack: @escaping () -> Void, onDone: @escaping () -> Void) {
        _model = State(initialValue: PumpPanelModel(store: store, mode: .past, unit: unit))
        self.onBack = onBack
        self.onDone = onDone
    }

    var body: some View {
        EntryPanel(title: Tracker.pump.title, icon: .pump, tint: Palette.pump, canSave: model.canSave,
                   identifierPrefix: "pump", onCancel: onBack, cancelTitle: EditChromeCopy.back,
                   onSave: save, onDelete: nil) {
            PumpFields(model: model, isShowingDetails: $isShowingDetails)
        }
    }

    private func save() {
        do {
            try model.save()
            errorReporter.clear()
            onDone()
        } catch {
            errorReporter.report(ErrorBannerCopy.saveFailed(what: ErrorSubject.pump), error: error,
                                 category: AppLog.store)
        }
    }
}
